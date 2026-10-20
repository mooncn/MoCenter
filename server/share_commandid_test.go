package main

import (
	"strings"
	"testing"
)

func TestValidShareCommandID(t *testing.T) {
	ok := []string{
		"share-it-1760000000000",
		"share-abc_DEF.1",
	}
	for _, id := range ok {
		if !validShareCommandID(id) {
			t.Errorf("合法分享命令 ID 被拒: %q", id)
		}
	}
	bad := []string{
		"",
		"share-",
		"it-1760000000000",
		"batch-1",
		"share-; rm -rf /",
		"share-$(id)",
		"share-" + strings.Repeat("a", 60),
	}
	for _, id := range bad {
		if validShareCommandID(id) {
			t.Errorf("非法分享命令 ID 被放行: %q", id)
		}
	}
}
