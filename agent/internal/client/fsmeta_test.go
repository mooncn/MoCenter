package client

import "testing"

// TestParseFsMode 八进制权限解析（跨平台）。
func TestParseFsMode(t *testing.T) {
	ok := map[string]uint32{
		"755": 0o755, "0644": 0o644, "0o644": 0o644, "600": 0o600, "7777": 0o7777, "0": 0,
	}
	for in, want := range ok {
		got, err := parseFsMode(in)
		if err != nil || uint32(got) != want {
			t.Errorf("parseFsMode(%q) = %o, %v; want %o", in, uint32(got), err, want)
		}
	}
	for _, bad := range []string{"", "999", "abc", "17777", "-1", "0x1ff"} {
		if _, err := parseFsMode(bad); err == nil {
			t.Errorf("parseFsMode(%q) 应报错", bad)
		}
	}
}

// TestResolveFsOwner 属主/组解析：数字 ID、空值、未知名称。
func TestResolveFsOwner(t *testing.T) {
	uid, gid, err := resolveFsOwner("1000", "1000")
	if err != nil || uid != 1000 || gid != 1000 {
		t.Fatalf("数字 ID 解析失败: %d %d %v", uid, gid, err)
	}
	uid, gid, err = resolveFsOwner("1000", "")
	if err != nil || uid != 1000 || gid != -1 {
		t.Fatalf("只改所有者时应把 gid 置 -1: %d %d %v", uid, gid, err)
	}
	if _, _, err := resolveFsOwner("", ""); err == nil {
		t.Error("两者都为空应报错")
	}
	if _, _, err := resolveFsOwner("no-such-user-xyz", ""); err == nil {
		t.Error("未知用户应报错")
	}
	if _, _, err := resolveFsOwner("", "no-such-group-xyz"); err == nil {
		t.Error("未知分组应报错")
	}
}
