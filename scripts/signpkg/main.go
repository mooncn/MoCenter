// 命令 signpkg —— 生成升级包签名密钥、给 Agent 升级包签名与验签。
//
// 为什么需要它：Agent 侧的升级验签是**强制**的（没有"允许无签名升级"的开关），
// 而验签的前提是有人在离线机器上签过。少了这个工具，"必须验签"就只是一句
// 会让功能失效的话术。
//
// 消息格式（必须与 agent/internal/upgrade.signedMessage 逐字节一致）：
//
//	"MoCenter-Upgrade-v1\n" + 目标版本号 + "\n" + 包内容
//
// 版本号进消息是有意为之：只对内容签名的话，一份合法签名的**旧包**
// 可以配上任意 version 复用，等于允许把机群降级钉死（评估项 M-10）。
// 两处各自有断言测试钉住这个字面量，改任何一边都会红。
//
// 私钥只应存在于离线机器/CI 的密钥存储里；公钥（hex）才进服务端与 Agent 配置。
//
// 用法：
//
//	go run . -gen -key-out upgrade.key -pub-out upgrade.pub   # 首次：出密钥对
//	go run . -sign -key upgrade.key -version 1.0.2 mocenter-agent-linux-amd64
//	go run . -verify -pubkey upgrade.pub -version 1.0.2 \
//	     -sha256 <摘要> -signature <签名> mocenter-agent-linux-amd64
package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
)

// signatureDomain 与 Agent 侧常量的镜像（跨模块无法共享，靠测试钉住）。
const signatureDomain = "MoCenter-Upgrade-v1\n"

func main() {
	fs := flag.NewFlagSet("signpkg", flag.ExitOnError)
	var (
		gen     = fs.Bool("gen", false, "生成 Ed25519 密钥对（私钥 0600）")
		sign    = fs.Bool("sign", false, "给升级包签名")
		verify  = fs.Bool("verify", false, "校验已有签名（发布前自检）")
		keyOut  = fs.String("key-out", "upgrade.key", "-gen：私钥输出路径")
		pubOut  = fs.String("pub-out", "upgrade.pub", "-gen：公钥输出路径（hex，可直接填进配置）")
		key     = fs.String("key", "", "私钥文件（hex）")
		pubkey  = fs.String("pubkey", "", "公钥文件/hex（-verify 用）")
		version = fs.String("version", "", "目标版本号，如 1.0.2（参与签名）")
		shaWant = fs.String("sha256", "", "-verify：期望的包摘要")
		sigWant = fs.String("signature", "", "-verify：待验签名（hex）")
	)
	_ = fs.Parse(os.Args[1:])

	args := fs.Args()
	if *gen {
		if err := generate(*keyOut, *pubOut); err != nil {
			fail(err)
		}
		return
	}
	if *version == "" {
		fail(errors.New("-version 必填：版本号参与签名，缺了它就等于回到" +
			"“一份合法旧包可以配任意版本号”的老问题"))
	}
	if len(args) != 1 {
		fail(errors.New("需要一个升级包文件路径"))
	}
	content, err := os.ReadFile(args[0])
	if err != nil {
		fail(fmt.Errorf("读取升级包失败: %w", err))
	}
	if len(content) == 0 {
		fail(errors.New("升级包为空"))
	}
	sum := sha256.Sum256(content)
	sha := hex.EncodeToString(sum[:])

	switch {
	case *sign:
		sig, serr := signFile(*key, *version, content)
		if serr != nil {
			fail(serr)
		}
		fmt.Printf("文件      : %s\n版本      : %s\nsha256    : %s\nsignature : %s\n", args[0], *version, sha, sig)
		// 直接给出可粘贴的请求体，省掉"手工拼 JSON 拼错"这一类返工
		fmt.Printf("\nPOST /api/upgrade\n{\"version\":%q,\"url\":\"https://<你的下载地址>\","+
			"\"sha256\":%q,\"signature\":%q,\"agent_id\":\"<agent_id>\"}\n", *version, sha, sig)
	case *verify:
		pub, perr := readKey(*pubkey)
		if perr != nil {
			fail(perr)
		}
		if err := verifyFile(pub, *version, *sigWant, *shaWant, content); err != nil {
			fail(err)
		}
		fmt.Println("OK 签名与摘要都匹配")
	default:
		fail(errors.New("请用 -gen / -sign / -verify 之一（-h 看用法）"))
	}
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "错误:", err)
	os.Exit(1)
}

// generate 产出密钥对。私钥 0600：这把钥匙能决定你的机群执行什么二进制。
func generate(keyPath, pubPath string) error {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("生成密钥失败: %w", err)
	}
	if err := os.WriteFile(keyPath, []byte(hex.EncodeToString(priv.Seed())+"\n"), 0600); err != nil {
		return fmt.Errorf("写私钥失败: %w", err)
	}
	if err := os.WriteFile(pubPath, []byte(hex.EncodeToString(pub)+"\n"), 0644); err != nil {
		return fmt.Errorf("写公钥失败: %w", err)
	}
	fmt.Printf("私钥: %s（0600，**只在离线机器上保管**，不要提交、不要进备份包）\n公钥: %s\n", keyPath, pubPath)
	fmt.Printf("把公钥内容填到服务端 -upgrade-pubkey（一键部署会写进 Agent 配置）\n公钥 hex: %s\n", hex.EncodeToString(pub))
	return nil
}

// loadPrivate 从 hex 文件读私钥（seed 或直接私钥两种写法都接受）。
func loadPrivate(spec string) (ed25519.PrivateKey, error) {
	raw, err := readHex(spec)
	if err != nil {
		return nil, fmt.Errorf("私钥不可用: %w", err)
	}
	switch len(raw) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(raw), nil
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(raw), nil
	default:
		return nil, fmt.Errorf("私钥长度应为 %d 或 %d 字节，实际 %d",
			ed25519.SeedSize, ed25519.PrivateKeySize, len(raw))
	}
}

// readHex 接受 "hex 串"、"文件路径"或"文件路径里存的 hex"。
func readHex(spec string) ([]byte, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, errors.New("密钥为空")
	}
	if fi, statErr := os.Stat(spec); statErr == nil && !fi.IsDir() {
		b, rerr := os.ReadFile(spec)
		if rerr != nil {
			return nil, rerr
		}
		spec = strings.TrimSpace(string(b))
	}
	b, err := hex.DecodeString(strings.TrimPrefix(spec, "ed25519:"))
	if err != nil {
		return nil, fmt.Errorf("不是合法 hex: %w", err)
	}
	return b, nil
}

func readKey(spec string) (ed25519.PublicKey, error) {
	raw, err := readHex(spec)
	if err != nil {
		return nil, fmt.Errorf("公钥不可用: %w", err)
	}
	if len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("公钥长度应为 %d 字节，实际 %d", ed25519.PublicKeySize, len(raw))
	}
	return ed25519.PublicKey(raw), nil
}

// signedMessage 待签消息：用途前缀 + 版本 + 包内容。
func signedMessage(target string, content []byte) []byte {
	buf := make([]byte, 0, len(signatureDomain)+len(target)+1+len(content))
	buf = append(buf, signatureDomain...)
	buf = append(buf, target...)
	buf = append(buf, '\n')
	return append(buf, content...)
}

func signFile(keySpec, target string, content []byte) (string, error) {
	priv, err := loadPrivate(keySpec)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(ed25519.Sign(priv, signedMessage(target, content))), nil
}

func verifyFile(pub ed25519.PublicKey, target, sigHex, shaWant string, content []byte) error {
	sum := sha256.Sum256(content)
	if got := hex.EncodeToString(sum[:]); shaWant != "" && got != strings.ToLower(shaWant) {
		return fmt.Errorf("摘要不符：期望 %s，实际 %s", shaWant, got)
	}
	sig, err := hex.DecodeString(strings.TrimSpace(sigHex))
	if err != nil || len(sig) != ed25519.SignatureSize {
		return errors.New("签名格式不合法（应为 128 位 hex）")
	}
	if !ed25519.Verify(pub, signedMessage(target, content), sig) {
		return errors.New("签名校验失败")
	}
	return nil
}
