//go:build windows

package main

import (
	"reflect"
	"strings"
	"testing"
)

// TestRedactedArgs 覆盖安装失败日志的脱敏（H-9）：注册密钥与登录口令绝不能
// 跟着 argv 落进安装目录里那份继承来可读的 mocenter-deploy.log。
func TestRedactedArgs(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"旗标与值分离", []string{"install", "-role", "agent", "-agent-key", "SECRET", "-password", "PW"},
			[]string{"install", "-role", "agent", "-agent-key", "***", "-password", "***"}},
		{"等号形式", []string{"install", "-agent-key=SECRET", "--password=PW", "-extra=-x -auth-token=SECRET"},
			[]string{"install", "-agent-key=***", "--password=***", "-extra=***"}},
		{"口令文件只记路径", []string{"install", "-role", "server", "-password-file", `C:\tmp\pw.txt`},
			[]string{"install", "-role", "server", "-password-file", `C:\tmp\pw.txt`}},
		{"值本身像旗标也照脱敏", []string{"install", "-password", "-agent-key", "X"},
			[]string{"install", "-password", "***", "X"}},
		{"无敏感旗标原样保留", []string{"status", "-role", "all"}, []string{"status", "-role", "all"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := redactedArgs(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("redactedArgs(%v) =\n  %v\n期望\n  %v", c.in, got, c.want)
			}
			for _, a := range got {
				if strings.Contains(a, "SECRET") || strings.Contains(a, "PW") {
					t.Errorf("输出里仍有凭据明文: %v", got)
				}
			}
		})
	}
}
