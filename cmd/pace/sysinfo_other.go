//go:build !darwin && !linux

package main

import (
	"errors"
	"net"
)

func totalMemory() int64 { return 0 }

func peerPID(*net.UnixConn) (int, error) { return 0, errors.New("peer pid not supported on this OS") }
