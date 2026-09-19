// MMDB 离线地理库 provider（GeoLite2 / IPinfo / DB-IP 等 MaxMind DB 格式）。
//
// 这是「离线本地 GeoIP」需求的实现：此前节点地区完全依赖 3 个外部 HTTP 接口
// （api.iping.cc / ip-api.com / ipwho.is），在以下场景会整体失效：
//
//   - 服务端无外网出口（纯内网部署 / 白名单出网）→ 全站节点的地区都是空的；
//   - 一批 Agent 同时上线 → 免费接口按分钟限流，互相挤掉额度，
//     表现为"某个时间点之后新上线的机器都没有地区"；
//   - 接口自身波动 / IPv6 不受支持（api.iping.cc 对 IPv6 恒返回 code=500）。
//
// 因此把 MMDB 放在**链路最前面**（本地文件查询，微秒级、无网络、无配额），
// 命中即返回；未配置数据库文件时完全不影响既有三源降级逻辑。
//
// 支持的库与字段差异：
//
//	GeoLite2-Country.mmdb  country.iso_code / country.names
//	GeoLite2-City.mmdb     额外有 subdivisions / city / location
//	DB-IP / IPinfo 的 Country/City 库字段名基本兼容上述结构
//
// 刻意**不引入** City 级别特有的字段（如邮编、ISP）到公开视图：
// 公开页只需要国家/省/市，多取只会增加泄露面。
package geo

import (
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"sync"

	"github.com/oschwald/maxminddb-golang"

	"mocenter/server/internal/protocol"
)

// mmdbRecord MMDB 记录结构（兼容 GeoLite2 Country / City 两种库）。
//
// 用结构体而不是 map 反查：maxminddb 的 Decode 对结构体是零分配的，
// 而 map 反查每条都要建 map、做字符串哈希 —— 节点数量上百时差异明显。
type mmdbRecord struct {
	Country struct {
		ISOCode string `maxminddb:"iso_code"`
		Names   struct {
			En string `maxminddb:"en"`
			Zh string `maxminddb:"zh-CN"`
		} `maxminddb:"names"`
	} `maxminddb:"country"`
	RegisteredCountry struct {
		ISOCode string `maxminddb:"iso_code"`
	} `maxminddb:"registered_country"`
	Subdivisions []struct {
		Names struct {
			En string `maxminddb:"en"`
			Zh string `maxminddb:"zh-CN"`
		} `maxminddb:"names"`
	} `maxminddb:"subdivisions"`
	City struct {
		Names struct {
			En string `maxminddb:"en"`
			Zh string `maxminddb:"zh-CN"`
		} `maxminddb:"names"`
	} `maxminddb:"city"`
	Location struct {
		Latitude  float64 `maxminddb:"latitude"`
		Longitude float64 `maxminddb:"longitude"`
	} `maxminddb:"location"`
}

// MMDBProvider 离线 MMDB 查询器。
//
// 使用 maxminddb.Reader 是**并发安全**的（内部 mmap + 只读），
// 因此不需要对查询加锁；只有热重载（Reopen）时才用写锁换句柄。
type MMDBProvider struct {
	mu     sync.RWMutex
	reader *maxminddb.Reader
	path   string
	// loaded 是否已成功加载（未加载 = 未配置或加载失败，查询应直接返回 nil）
	loaded bool
}

// NewMMDBProvider 打开 MMDB 文件。path 为空或文件不存在时返回
// (provider, nil) 且 provider.Loaded()==false —— **刻意不返回错误**：
// 离线库是可选的增强项，缺了它必须优雅降级到外部 HTTP 源，
// 而不是让服务端起不来。
func NewMMDBProvider(path string) (*MMDBProvider, error) {
	p := &MMDBProvider{}
	if strings.TrimSpace(path) == "" {
		return p, nil
	}
	if err := p.Reopen(path); err != nil {
		return p, err
	}
	return p, nil
}

// Reopen 重新打开（热重载）MMDB 文件。
//
// 用途：运维更新了 GeoLite2 数据文件后，无需重启服务端 ——
// 调用一次热重载即可让全部节点的地区解析走新库。
func (p *MMDBProvider) Reopen(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return fmt.Errorf("MMDB 路径为空")
	}
	// 先做一次显式 os.Stat：区分"文件不存在"（部署未放库，属正常降级）
	// 与"文件损坏/格式不对"（配置错误，必须让运维看到），否则两者
	// 都表现为一句 Open 失败，排查时无从下手。
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("MMDB 文件不可用 %s: %w", path, err)
	}
	if fi.IsDir() {
		return fmt.Errorf("MMDB 路径是目录而不是文件: %s", path)
	}
	r, err := maxminddb.Open(path)
	if err != nil {
		return fmt.Errorf("打开 MMDB 失败 %s: %w", path, err)
	}
	p.mu.Lock()
	old := p.reader
	p.reader, p.path, p.loaded = r, path, true
	p.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	log.Printf("[geo] 已加载离线 GeoIP 库: %s（数据库类型 %s）", path, r.Metadata.DatabaseType)
	return nil
}

// Loaded 是否已成功加载离线库。
func (p *MMDBProvider) Loaded() bool {
	if p == nil {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.loaded && p.reader != nil
}

// Path 返回当前加载的库文件路径（未加载时为空）。
func (p *MMDBProvider) Path() string {
	if p == nil {
		return ""
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.path
}

// Close 释放句柄（进程退出时调用即可，非必须）。
func (p *MMDBProvider) Close() {
	if p == nil {
		return
	}
	p.mu.Lock()
	r := p.reader
	p.reader, p.loaded = nil, false
	p.mu.Unlock()
	if r != nil {
		_ = r.Close()
	}
}

// Lookup 查询离线库。未加载、未命中、地址非法时返回 nil（交由下一个源）。
func (p *MMDBProvider) Lookup(ip string) *protocol.GeoInfo {
	if !p.Loaded() {
		return nil
	}
	addr := net.ParseIP(trimIP(ip))
	if addr == nil {
		return nil
	}
	p.mu.RLock()
	reader := p.reader
	p.mu.RUnlock()
	if reader == nil {
		return nil
	}
	var rec mmdbRecord
	if err := reader.Lookup(addr, &rec); err != nil {
		// 单条记录解析失败（库版本与结构不匹配、IP 不在库中）不该影响整批查询，
		// 记一次日志后按"未命中"处理，让链路继续降级到下一个源。
		log.Printf("[geo] MMDB 查询 %s 失败: %v", ip, err)
		return nil
	}
	code := strings.ToUpper(strings.TrimSpace(rec.Country.ISOCode))
	if code == "" {
		// 部分库对国家留空、只填 registered_country（如 SAT 段地址）；
		// 取归属国是更合理的结果，但也可能同样为空 —— 那时视为未命中。
		code = strings.ToUpper(strings.TrimSpace(rec.RegisteredCountry.ISOCode))
	}
	if code == "" {
		return nil
	}
	g := &protocol.GeoInfo{
		Country:     firstNonEmpty(rec.Country.Names.Zh, rec.Country.Names.En),
		CountryCode: code,
		Latitude:    rec.Location.Latitude,
		Longitude:   rec.Location.Longitude,
	}
	if len(rec.Subdivisions) > 0 {
		g.Region = firstNonEmpty(rec.Subdivisions[0].Names.Zh, rec.Subdivisions[0].Names.En)
	}
	g.City = firstNonEmpty(rec.City.Names.Zh, rec.City.Names.En)
	// 国家名可能为空（库只有 ISO 代码）：用内置国名表补上，
	// 否则公开页会显示成"国旗有、国家名空"。
	if strings.TrimSpace(g.Country) == "" {
		g.Country = countryNameByCode(code)
	}
	return normalizeGeo(g)
}

// countryNameByCode 由 ISO 代码反查英文国名（依赖 countries.go 中的对照表）。
func countryNameByCode(code string) string {
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" {
		return ""
	}
	return countryEnglishByCode(code)
}
