package nix

import (
	"encoding/json"
	"io"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	log "github.com/hashicorp/go-hclog"
)

type journaldLine struct {
	Message          string `json:"MESSAGE"`
	SyslogIdentifier string `json:"SYSLOG_IDENTIFIER"`
}

type OOM struct {
	MachineID string
	Task      string
	PID       uint64
}

type OOMListener struct {
	log        log.Logger
	register   chan *registration
	deregister chan string
	oom        chan *OOM
}

func NewOOMListener(log log.Logger) *OOMListener {
	listener := &OOMListener{
		log:        log,
		register:   make(chan *registration, 10),
		deregister: make(chan string, 10),
		oom:        make(chan *OOM, 10),
	}

	go listener.loop()
	go listener.Start()

	return listener
}

type registration struct {
	id string
	c  chan *OOM
	t  time.Time
}

func (self OOMListener) loop() {
	ids := map[string]*registration{}

	for {
		select {
		case reg := <-self.register:
			self.log.Debug("Register listening for OOM of", "id", reg.id)
			ids[reg.id] = reg
		case id := <-self.deregister:
			self.log.Debug("Deregister listening for OOM of", "id", id)
			delete(ids, id)
		case oom := <-self.oom:
			self.log.Debug("Received OOM of", "id", oom.MachineID)
			reg, found := ids[oom.MachineID]
			if found && (reg != nil) {
				reg.c <- oom
			}
			delete(ids, oom.MachineID)
		}
	}
}

func (self OOMListener) Register(machineID string) chan *OOM {
	c := make(chan *OOM)
	self.register <- &registration{id: machineID, c: c, t: time.Now()}
	return c
}

func (self OOMListener) Deregister(machineID string) {
	self.deregister <- machineID
}

func (self OOMListener) Start() {
	for {
		self.journalctlListener()
	}
}

func (self OOMListener) journalctlListener() {
	cmd := exec.Command("journalctl", "-e", "-f", "-k", "-o", "json", "-g", "oom-kill:")

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		panic(err)
	}

	journaldChan := make(chan *journaldLine)
	go self.journalctlReader(stdout, journaldChan)

	go func() {
		err = cmd.Run()
		if err != nil {
			self.log.Error("failed running journalctl", err)
		}
	}()

	for line := range journaldChan {
		self.parseLine(line.Message)
	}
}

func (self OOMListener) journalctlReader(reader io.ReadCloser, out chan *journaldLine) {
	defer reader.Close()

	dec := json.NewDecoder(reader)

	for {
		line := &journaldLine{}
		err := dec.Decode(line)
		if err != nil {
			if err == io.EOF {
				break
			}
			panic(err)
		}

		if line.SyslogIdentifier == "kernel" {
			out <- line
		}
	}

	close(out)
}

func (self OOMListener) parseLine(line string) {
	if strings.HasPrefix(line, "oom-kill:") {
		// oom-kill:constraint=CONSTRAINT_MEMCG,nodemask=(null),cpuset=payload,mems_allowed=0,oom_memcg=/machine.slice/machine-oom\\x2d9706e99d\\x2d0658\\x2d2cf0\\x2d7f06\\x2d4c339d36c355.scope,task_memcg=/machine.slice/machine-oom\\x2d9706e99d\\x2d0658\\x2d2cf0\\x2d7f06\\x2d4c339d36c355.scope/payload,task=bash,pid=980323,uid=0

		var pid uint64
		var task string
		var id string

		for _, field := range strings.Split(line, ",") {
			parts := strings.SplitN(field, "=", 2)

			if len(parts) != 2 {
				self.log.Error("Unexpected format of oom-kill", "line", line)
			}

			switch parts[0] {
			case "oom_memcg":
				r := regexp.MustCompile(`^/machine\.slice/machine-(.+)\.scope$`)
				scope := strings.Replace(parts[1], "\\x2d", "-", -1)
				match := r.FindStringSubmatch(scope)
				if len(match) == 0 {
					self.log.Error("Unexpected format of oom_memcg", "line", line)
				}

				id = match[1]
			case "pid":
				var err error
				pid, err = strconv.ParseUint(parts[1], 10, 64)
				if err != nil {
					self.log.Error("Unexpected format of pid", "line", line, "err", err)
				}
			case "task":
				task = parts[1]
			}
		}

		oom := &OOM{PID: pid, Task: task, MachineID: id}
		self.oom <- oom
	} else if strings.HasPrefix(line, "Memory cgroup out of memory:") {
		// TODO: parse this line and add info about memory?
		// Memory cgroup out of memory: Killed process 2933082 (bash) total-vm:1051956kB, anon-rss:101820kB, file-rss:1632kB, shmem-rss:0kB, UID:0 pgtables:252kB oom_score_adj:0
	} else if strings.HasPrefix(line, "oom_reaper:") {
		// NOTE: nothing particularly useful about this line, but it shows resources after the kill.
		// oom_reaper: reaped process 2931684 (bash), now anon-rss:0kB, file-rss:0kB, shmem-rss:0kB
	}
}
