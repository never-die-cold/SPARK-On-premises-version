//go:build !windows

package main

import "syscall"

func procAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{}
}
