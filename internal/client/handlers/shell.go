//go:build !windows
// +build !windows

package handlers

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"

	"github.com/NHAS/reverse_ssh/internal"
	"github.com/NHAS/reverse_ssh/pkg/logger"
	"github.com/creack/pty"
	"golang.org/x/crypto/ssh"
)

var shells []string

func init() {

	var potentialShells []string
	file, err := os.Open("/etc/shells")
	if err == nil {
		defer file.Close()
		scanner := bufio.NewScanner(file)
		for scanner.Scan() {
			line := scanner.Text()

			if len(line) > 0 && line[0] == '#' || strings.TrimSpace(line) == "" {
				continue
			}
			shells = append(shells, strings.TrimSpace(line))
		}
	} else {
		//If the host did not have a /etc/shells, guess a few common shells
		potentialShells = []string{
			"/bin/bash",
			"/bin/zsh",
			"/bin/ash",
			"/bin/sh",
		}

	}
	for _, s := range potentialShells {

		if stats, err := os.Stat(s); err != nil && (os.IsNotExist(err) || !stats.IsDir()) {
			continue

		}
		shells = append(shells, s)
	}

}

//This basically handles exactly like a SSH server would
func shell(user *internal.User, connection ssh.Channel, requests <-chan *ssh.Request, log logger.Logger) {

	if user.Pty == nil {
		fmt.Fprintf(connection, "Shell without pty not allowed.")
		return
	}

	if os.Getuid() != 0 {
		path, err := os.Executable()
		if err != nil {

		} else {
			var i syscall.Stat_t
			err := syscall.Stat(path, &i)
			if err != nil {
				syscall.Setuid(0)
				syscall.Setgid(0)
			} else {
				if os.Geteuid() > int(i.Uid) {
					syscall.Setuid(int(i.Uid))
				}

				if os.Getegid() > int(i.Gid) {
					syscall.Setgid(int(i.Gid))
				}
			}
		}
	}

	path := ""
	if len(shells) != 0 {
		path = shells[0]
	}

	// Fire up a shell for this session
	shell := exec.Command(path)
	shell.Env = os.Environ()
	shell.Env = append(shell.Env, "TERM="+user.Pty.Term)

	close := func() {
		connection.Close()
		if shell.Process != nil {

			err := shell.Process.Kill()
			if err != nil {
				log.Warning("Failed to kill shell(%s)", err)
			}
		}

		log.Info("Session closed")
	}

	// Allocate a terminal for this channel
	log.Info("Creating pty...")
	shellf, err := pty.StartWithSize(shell, &pty.Winsize{Cols: uint16(user.Pty.Columns), Rows: uint16(user.Pty.Rows)})
	if err != nil {
		log.Info("Could not start pty (%s)", err)
		close()
		return
	}

	//pipe session to bash and visa-versa
	var once sync.Once
	go func() {
		io.Copy(connection, shellf)
		once.Do(close)
	}()
	go func() {
		io.Copy(shellf, connection)
		once.Do(close)
	}()

	go func() {
		for req := range requests {
			switch req.Type {

			case "window-change":
				w, h := internal.ParseDims(req.Payload)
				err = pty.Setsize(shellf, &pty.Winsize{Cols: uint16(w), Rows: uint16(h)})
				if err != nil {
					log.Warning("Unable to set terminal size: %s", err)
				}

			default:
				log.Warning("Unknown request %s", req.Type)
				if req.WantReply {
					req.Reply(false, nil)
				}
			}
		}
	}()

	defer once.Do(close)
	shell.Wait()

}
