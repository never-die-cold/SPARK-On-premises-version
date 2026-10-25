package main

import (
	"syscall"
)

// procAttr hides the console window of supervised llama.cpp processes on
// Windows so agents running at login do not flash windows on every restart.
func procAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{HideWindow: true}
}
