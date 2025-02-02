// Copyright 2018 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may not
// use this file except in compliance with the License. A copy of the
// License is located at
//
// http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND,
// either express or implied. See the License for the specific language governing
// permissions and limitations under the License.
//
// +build darwin freebsd linux netbsd openbsd

// Package shell implements session shell plugin.
package shell

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aws/amazon-ssm-agent/agent/appconfig"
	agentContracts "github.com/aws/amazon-ssm-agent/agent/contracts"
	"github.com/aws/amazon-ssm-agent/agent/log"
	mgsConfig "github.com/aws/amazon-ssm-agent/agent/session/config"
	"github.com/aws/amazon-ssm-agent/agent/session/utility"
	"github.com/kr/pty"
)

var ptyFile *os.File

const (
	termEnvVariable       = "TERM=xterm-256color"
	langEnvVariable       = "LANG=C.UTF-8"
	langEnvVariableKey    = "LANG"
	startRecordSessionCmd = "script"
	newLineCharacter      = "\n"
	screenBufferSizeCmd   = "screen -h %d%s"
	homeEnvVariable       = "HOME=/home/" + appconfig.DefaultRunAsUserName
)

//StartPty starts pty and provides handles to stdin and stdout
func StartPty(log log.T, runAsSsmUser bool, shellCmd string) (stdin *os.File, stdout *os.File, err error) {
	log.Info("Starting pty")
	//Start the command with a pty
	var cmd *exec.Cmd
	if strings.TrimSpace(shellCmd) == "" {
		cmd = exec.Command("sh")
	} else {
		commandArgs := append(utility.ShellPluginCommandArgs, shellCmd)
		cmd = exec.Command("sh", commandArgs...)
	}

	//TERM is set as linux by pty which has an issue where vi editor screen does not get cleared.
	//Setting TERM as xterm-256color as used by standard terminals to fix this issue
	cmd.Env = append(os.Environ(),
		termEnvVariable,
		homeEnvVariable,
	)

	//If LANG environment variable is not set, shell defaults to POSIX which can contain 256 single-byte characters.
	//Setting C.UTF-8 as default LANG environment variable as Session Manager supports UTF-8 encoding only.
	langEnvVariableValue := os.Getenv(langEnvVariableKey)
	if langEnvVariableValue == "" {
		cmd.Env = append(cmd.Env, langEnvVariable)
	}

	// Get the uid and gid of the runas user.
	if runAsSsmUser {
		// Create ssm-user before starting a session.
		u := &utility.SessionUtil{}
		u.CreateLocalAdminUser(log)

		uid, gid, groups, err := getUserCredentials(log)
		if err != nil {
			return nil, nil, err
		}
		cmd.SysProcAttr = &syscall.SysProcAttr{}
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: uid, Gid: gid, Groups: groups, NoSetGroups: false}
	}

	ptyFile, err = pty.Start(cmd)
	if err != nil {
		log.Errorf("Failed to start pty: %s\n", err)
		return nil, nil, fmt.Errorf("Failed to start pty: %s\n", err)
	}

	return ptyFile, ptyFile, nil
}

//Stop closes pty file.
func Stop(log log.T) (err error) {
	log.Info("Stopping pty")
	if err := ptyFile.Close(); err != nil {
		return fmt.Errorf("unable to close ptyFile. %s", err)
	}
	return nil
}

//SetSize sets size of console terminal window.
func SetSize(log log.T, ws_col, ws_row uint32) (err error) {
	winSize := pty.Winsize{
		Cols: uint16(ws_col),
		Rows: uint16(ws_row),
	}

	if err := pty.Setsize(ptyFile, &winSize); err != nil {
		return fmt.Errorf("set pty size failed: %s", err)
	}
	return nil
}

// getUserCredentials returns the uid, gid and groups associated to the runas user.
func getUserCredentials(log log.T) (uint32, uint32, []uint32, error) {
	uidCmdArgs := append(utility.ShellPluginCommandArgs, fmt.Sprintf("id -u %s", appconfig.DefaultRunAsUserName))
	cmd := exec.Command(utility.ShellPluginCommandName, uidCmdArgs...)
	out, err := cmd.Output()
	if err != nil {
		log.Errorf("Failed to retrieve uid for %s: %v", appconfig.DefaultRunAsUserName, err)
		return 0, 0, nil, err
	}

	uid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		log.Errorf("%s not found: %v", appconfig.DefaultRunAsUserName, err)
		return 0, 0, nil, err
	}

	gidCmdArgs := append(utility.ShellPluginCommandArgs, fmt.Sprintf("id -g %s", appconfig.DefaultRunAsUserName))
	cmd = exec.Command(utility.ShellPluginCommandName, gidCmdArgs...)
	out, err = cmd.Output()
	if err != nil {
		log.Errorf("Failed to retrieve gid for %s: %v", appconfig.DefaultRunAsUserName, err)
		return 0, 0, nil, err
	}

	gid, err := strconv.Atoi(strings.TrimSpace(string(out)))
	if err != nil {
		log.Errorf("%s not found: %v", appconfig.DefaultRunAsUserName, err)
		return 0, 0, nil, err
	}

	// Get the list of associated groups
	groupNamesCmdArgs := append(utility.ShellPluginCommandArgs, fmt.Sprintf("groups %s", appconfig.DefaultRunAsUserName))
	cmd = exec.Command(utility.ShellPluginCommandName, groupNamesCmdArgs...)
	out, err = cmd.Output()
	if err != nil {
		log.Errorf("Failed to retrieve groups for %s: %v", appconfig.DefaultRunAsUserName, err)
		return 0, 0, nil, err
	}

	groupNames := strings.Split(string(out), " ")
	var groupIds []uint32

	// Skip the first two elements. Group names start from the third element.
	// Format ex: ssm-user : ssm-user test
	for i := 2; i < len(groupNames); i++ {
		groupIdFromNameCmdArgs := append(utility.ShellPluginCommandArgs, fmt.Sprintf("getent group %s", groupNames[i]))
		cmd = exec.Command(utility.ShellPluginCommandName, groupIdFromNameCmdArgs...)
		out, err = cmd.Output()
		if err != nil {
			log.Errorf("Failed to retrieve group id for %s: %v", groupNames[i], err)
			return 0, 0, nil, err
		}

		// Get the third element from the array which contains the id and convert it to int
		// Format ex: test:x:1004:ssm-user
		groupIdFromName, err := strconv.Atoi(strings.TrimSpace(strings.Split(string(out), ":")[2]))
		if err != nil {
			log.Errorf("%s group id not found: %v", groupNames[i], err)
			return 0, 0, nil, err
		}

		groupIds = append(groupIds, uint32(groupIdFromName))
	}

	// Make sure they are non-zero valid positive ids
	if uid > 0 && gid > 0 {
		return uint32(uid), uint32(gid), groupIds, nil
	}

	return 0, 0, nil, errors.New("invalid uid and gid")
}

// generateLogData generates a log file with the executed commands.
func (p *ShellPlugin) generateLogData(log log.T, config agentContracts.Configuration) error {
	shadowShellInput, _, err := StartPty(log, false, "")
	if err != nil {
		return err
	}

	defer func() {
		if err := recover(); err != nil {
			if err = Stop(log); err != nil {
				log.Errorf("Error occured while closing pty: %v", err)
			}
		}
	}()

	time.Sleep(5 * time.Second)

	// Increase buffer size
	screenBufferSizeCmdInput := fmt.Sprintf(screenBufferSizeCmd, mgsConfig.ScreenBufferSize, newLineCharacter)
	shadowShellInput.Write([]byte(screenBufferSizeCmdInput))

	time.Sleep(5 * time.Second)

	// Start shell recording
	recordCmdInput := fmt.Sprintf("%s %s%s", startRecordSessionCmd, p.logFilePath, newLineCharacter)
	shadowShellInput.Write([]byte(recordCmdInput))

	time.Sleep(5 * time.Second)

	// Start shell logger
	loggerCmdInput := fmt.Sprintf("%s %s %t%s", appconfig.DefaultSessionLogger, p.ipcFilePath, false, newLineCharacter)
	shadowShellInput.Write([]byte(loggerCmdInput))

	// Sleep till the logger completes execution
	time.Sleep(time.Minute)

	exitCmdInput := fmt.Sprintf("%s%s", mgsConfig.Exit, newLineCharacter)

	// Exit start record command
	shadowShellInput.Write([]byte(exitCmdInput))

	// Sleep until start record command is exited successfully
	time.Sleep(30 * time.Second)

	// Exit screen buffer command
	shadowShellInput.Write([]byte(exitCmdInput))

	// Sleep till screen buffer command is exited successfully
	time.Sleep(5 * time.Second)

	// Exit shell
	shadowShellInput.Write([]byte(exitCmdInput))

	// Sleep till shell is exited successfully
	time.Sleep(5 * time.Second)

	// Close pty
	shadowShellInput.Close()

	// Sleep till the shell successfully exits before uploading
	time.Sleep(15 * time.Second)

	return nil
}
