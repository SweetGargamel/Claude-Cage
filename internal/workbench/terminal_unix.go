//go:build !windows

package workbench

import (
	"os"
	"os/exec"

	"github.com/creack/pty"
)

type unixTerminal struct {
	*os.File
	cmd *exec.Cmd
}

func startTerminal(cmd *exec.Cmd, cols, rows uint16) (terminalPTY, error) {
	f, err := pty.StartWithSize(cmd, &pty.Winsize{Cols: cols, Rows: rows})
	if err != nil {
		return nil, err
	}
	return &unixTerminal{File: f, cmd: cmd}, nil
}
func (p *unixTerminal) Resize(cols, rows uint16) error {
	return pty.Setsize(p.File, &pty.Winsize{Cols: cols, Rows: rows})
}
func (p *unixTerminal) Wait() error { return p.cmd.Wait() }
func (p *unixTerminal) Kill() error { return p.cmd.Process.Kill() }
