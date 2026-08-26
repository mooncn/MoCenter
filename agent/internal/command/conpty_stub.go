//go:build !windows

package command

import (
	"fmt"
	"os"
)

// conptySession 在非 Windows 平台为空壳，仅用于保持 interactive.go 分支可编译。
type conptySession struct{}

func (s *conptySession) Stdin() *os.File      { return nil }
func (s *conptySession) Stdout() *os.File     { return nil }
func (s *conptySession) Done() <-chan struct{} { return nil }
func (s *conptySession) Resize(_, _ int) error { return nil }
func (s *conptySession) Close()                {}

// startConPTYSession 在非 Windows 平台不可用。
func startConPTYSession(_ string, _ []string, _, _ int) (*conptySession, error) {
	return nil, fmt.Errorf("ConPTY 仅支持 Windows")
}