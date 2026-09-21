package main

import (
	"flag"
	"log"
)

// normalizeBlankFlags 与 server/flags.go 同实现（两个独立 go.mod，没有共享内部模块）。
//
// 把「取值为空串」的参数回落成它自己的默认值，返回被处理过的参数名，exclude 原样保留。
// 必须在读取/回写配置文件之前调用。
//
// 为什么需要它：mocenter-agent.service 的参数写成 `-server "${MOCENTER_SERVER}"`。引号是
// M-16 特意加的（不引号时值里带空格会被分词），副作用是 EnvironmentFile 里没配这个键时，
// 进程收到的不是「没有这个参数」而是「一个空 argv」，而 flag.Visit 会把它算成
// 「命令行显式给出」——自动回写于是用空串盖掉配置文件里已存的值，
// `-server ""` 能把 Agent 记住的服务端地址直接抹掉。
//
// Agent 侧没有任何一个字符串参数以空串为有效取值（帮助文本都写作「留空则…」），
// 所以调用时不排除任何参数。代价是「传空串清空某个配置项」不再生效：要清空改配置文件。
func normalizeBlankFlags(fs *flag.FlagSet, exclude ...string) map[string]bool {
	skip := make(map[string]bool, len(exclude))
	for _, name := range exclude {
		skip[name] = true
	}
	// 只对命令行真给过的参数出声告警：没给的参数默认值本来就是空串，逐个报会刷屏。
	given := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { given[f.Name] = true })

	done := map[string]bool{}
	fs.VisitAll(func(f *flag.Flag) {
		if skip[f.Name] || f.Value.String() != "" {
			return
		}
		if f.DefValue != "" {
			if err := f.Value.Set(f.DefValue); err != nil {
				log.Printf("警告: 参数 -%s 回落默认值 %q 失败: %v（按空值继续）", f.Name, f.DefValue, err)
				return
			}
		}
		done[f.Name] = true
		if given[f.Name] {
			log.Printf("参数 -%s 传入空值，按未配置处理（默认 %q）", f.Name, f.DefValue)
		}
	})
	return done
}

// clearBlankExplicit 从「命令行显式给出」集合里去掉被归一化的参数，
// 免得它们的空值回写进配置文件、盖掉运维已经配好的值。
func clearBlankExplicit(explicit, blank map[string]bool) {
	for name := range blank {
		delete(explicit, name)
	}
}
