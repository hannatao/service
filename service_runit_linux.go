package service

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"text/template"
	"time"
)

var runSvDir = "/etc/service"
var runItDir = "/etc/runit"

func init() {
	if val := os.Getenv("RUN_SV_DIR"); val != "" {
		runSvDir = val
	}
	if val := os.Getenv("RUN_IT_DIR"); val != "" {
		runItDir = val
	}
}

func isRunit() bool {
	if _, err := exec.LookPath("runsvdir"); err == nil {
		return true
	}
	return false
}

type runit struct {
	i        Interface
	svcDir   string
	platform string
	*Config
}

func (r *runit) String() string {
	if len(r.DisplayName) > 0 {
		return r.DisplayName
	}
	return r.Name
}

func (r *runit) Platform() string {
	return r.platform
}

func (r *runit) template() *template.Template {
	customScript := r.Option.string(optionRunItScript, "")

	if customScript != "" {
		return template.Must(template.New("").Funcs(tf).Parse(customScript))
	} else {
		return template.Must(template.New("").Funcs(tf).Parse(runItScript))
	}
}

func newRunItService(i Interface, platform string, c *Config) (Service, error) {
	s := &runit{
		i:        i,
		platform: platform,
		svcDir:   filepath.Join(runSvDir, c.Name),
		Config:   c,
	}
	return s, nil
}

var errNoUserServiceRunIt = errors.New("user services are not supported on RunIt")

func (r *runit) runItPath() (cp string, err error) {
	if r.Option.bool(optionUserService, optionUserServiceDefault) {
		err = errNoUserServiceOpenRC
		return
	}
	cp = filepath.Join(runItDir, r.Config.Name)
	return
}

func (r *runit) Install() error {
	runItPath, err := r.runItPath()
	if err != nil {
		return err
	}
	_, err = os.Stat(runItPath)
	if err == nil {
		return fmt.Errorf("Init already exists: %s", runItPath)
	}

	err = os.MkdirAll(runItPath, 0755)
	if err != nil {
		return err
	}
	runPath := filepath.Join(runItPath, "run")
	f, err := os.Create(runPath)
	if err != nil {
		return err
	}
	defer f.Close()

	err = os.Chmod(runPath, 0755)
	if err != nil {
		return err
	}

	path, err := r.execPath()
	if err != nil {
		return err
	}

	var to = &struct {
		*Config
		Path string
	}{
		r.Config,
		path,
	}

	err = r.template().Execute(f, to)
	if err != nil {
		return err
	}
	err = os.Symlink(runItPath, r.svcDir)
	if err != nil {
		return err
	}
	time.Sleep(6000 * time.Millisecond)
	return nil
}

func (r *runit) Uninstall() error {
	os.Remove(r.svcDir)
	runItPath, err := r.runItPath()
	if err != nil {
		return err
	}
	if err = os.RemoveAll(runItPath); err != nil {
		return err
	}
	return nil
}

func (r *runit) Logger(errs chan<- error) (Logger, error) {
	if system.Interactive() {
		return ConsoleLogger, nil
	}
	return r.SystemLogger(errs)
}

func (r *runit) SystemLogger(errs chan<- error) (Logger, error) {
	return newSysLogger(r.Name, errs)
}

func (r *runit) Run() (err error) {
	err = r.i.Start(r)
	if err != nil {
		return err
	}

	r.Option.funcSingle(optionRunWait, func() {
		var sigChan = make(chan os.Signal, 3)
		signal.Notify(sigChan, syscall.SIGTERM, os.Interrupt)
		<-sigChan
	})()

	return r.i.Stop(r)
}

func (r *runit) Status() (Status, error) {
	_, out, err := runWithOutput("sv", "status", r.svcDir)
	if err != nil {
		return StatusUnknown, err
	}
	if strings.Contains(out, "run") {
		return StatusRunning, nil
	} else if strings.Contains(out, "down") {
		return StatusStopped, nil
	}
	return StatusUnknown, nil
}

func (r *runit) GetPid() (uint32, error) {
	exitCode, out, err := runWithOutput("sv", "status", r.svcDir)
	if exitCode == 0 && err != nil {
		return 0, err
	}
	re := regexp.MustCompile(`pid ([0-9]+)`)
	matches := re.FindStringSubmatch(out)
	if len(matches) != 2 {
		return 0, errors.New("failed to match pid info")
	}
	pid, err := strconv.ParseUint(matches[1], 10, 32)
	if err != nil {
		return 0, err
	}
	return uint32(pid), nil
}

func (r *runit) Start() error {
	return run("sv", "up", r.svcDir)
}

func (r *runit) Stop() error {
	return run("sv", "down", r.svcDir)
}

func (r *runit) Restart() error {
	return run("sv", "restart", r.svcDir)
}

func (r *runit) runAction(action string) error {
	return r.run(action, r.Name)
}

func (r *runit) run(action string, args ...string) error {
	return run("sv", append([]string{action}, args...)...)
}

const runItScript = `#!/bin/sh
exec 2>&1
cd {{.WorkingDirectory}}
exec {{.Path|cmdEscape}} {{- if .Arguments }} {{range .Arguments}}{{.}} {{end}} {{- end }}
`
