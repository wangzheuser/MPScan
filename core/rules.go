package core

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// Rule 一条敏感信息匹配规则，可在 UI 中查看 / 修改 / 新增。
// 内置规则（Builtin=true）来自 DefaultRules，用户可以改其正则、等级、启用状态，
// 也可以新增自己的规则（Builtin=false）。所有改动持久化到 exe 同目录的 rules.json。
type Rule struct {
	ID         string `json:"id"`          // 唯一标识，内置规则固定，新增规则自动生成
	Category   string `json:"category"`    // 分类，显示在结果表"分类"列
	KeyName    string `json:"key_name"`    // 键名，显示在结果表"键名"列
	Level      string `json:"level"`       // 高危 / 中危 / 低危
	Pattern    string `json:"pattern"`     // 正则表达式（Go RE2 语法）
	ValueGroup int    `json:"value_group"` // 取值的捕获组序号，0=整个匹配
	Enabled    bool   `json:"enabled"`     // 是否参与扫描
	Builtin    bool   `json:"builtin"`     // 是否为内置规则
}

// compiledRule 编译后的规则（内部使用）
type compiledRule struct {
	Rule
	regex *regexp.Regexp
}

var (
	rulesMu       sync.RWMutex
	compiledCache []compiledRule
	rulesFilePath string
	rulesLoaded   bool
)

// DefaultRules 内置默认规则集。
// 这些正则与 2.0 版本内置的 sensitivePatterns 完全一致，行为不变；
// 如果发现误报，可直接在「规则管理」界面调整对应条目，无需改代码重新编译。
var DefaultRules = []Rule{
	// ══════ 微信小程序 ══════
	{ID: "wx-appsecret", Category: "微信·AppSecret", KeyName: "AppSecret", Level: "高危", ValueGroup: 1,
		Pattern: `(?i)["']?app[_-]?secret["']?\s*[:=]\s*["']([a-zA-Z0-9]{32})["']`},
	{ID: "wx-paykey", Category: "微信·支付密钥", KeyName: "MchKey/PaySignKey", Level: "高危", ValueGroup: 1,
		Pattern: `(?i)["']?(?:pay[_-]?sign[_-]?key|mch[_-]?key|partner[_-]?key|paykey)["']?\s*[:=]\s*["']([a-zA-Z0-9]{32})["']`},
	{ID: "wx-mchid", Category: "微信·商户号", KeyName: "MchID", Level: "中危", ValueGroup: 1,
		Pattern: `(?i)["']?(?:mch[_-]?id|mchid|merchant[_-]?id)["']?\s*[:=]\s*["']?(\d{8,15})["']?`},

	// ══════ 腾讯云 ══════
	{ID: "tx-secretid", Category: "腾讯云·SecretId", KeyName: "SecretId", Level: "高危", ValueGroup: 1,
		Pattern: `(?i)["']?secret[_-]?id["']?\s*[:=]\s*["']([A-Za-z0-9]{36,40})["']`},
	{ID: "tx-secretkey", Category: "腾讯云·SecretKey", KeyName: "SecretKey", Level: "高危", ValueGroup: 1,
		Pattern: `(?i)["']?secret[_-]?key["']?\s*[:=]\s*["']([A-Za-z0-9]{32,40})["']`},
	{ID: "tx-cos", Category: "腾讯云·COS桶", KeyName: "COSBucket", Level: "低危", ValueGroup: 1,
		Pattern: `([\w-]+-\d{9,13}\.cos\.[a-z0-9-]+\.myqcloud\.com)`},
	{ID: "tx-sms", Category: "腾讯云·短信", KeyName: "SMSAppKey", Level: "高危", ValueGroup: 1,
		Pattern: `(?i)["']?sms[_-]?(?:app[_-]?)?key["']?\s*[:=]\s*["']([a-zA-Z0-9]{32,40})["']`},

	// ══════ 阿里云 ══════
	{ID: "ali-akid", Category: "阿里云·AccessKeyId", KeyName: "AccessKeyId", Level: "高危", ValueGroup: 1,
		Pattern: `(?i)["']?access[_-]?key[_-]?id["']?\s*[:=]\s*["']([A-Za-z0-9]{20,24})["']`},
	{ID: "ali-aksecret", Category: "阿里云·AccessKeySecret", KeyName: "AccessKeySecret", Level: "高危", ValueGroup: 1,
		Pattern: `(?i)["']?access[_-]?key[_-]?secret["']?\s*[:=]\s*["']([A-Za-z0-9]{28,36})["']`},
	{ID: "ali-oss", Category: "阿里云·OSS端点", KeyName: "OSSEndpoint", Level: "低危", ValueGroup: 1,
		Pattern: `([\w-]+\.oss(?:-[a-z0-9-]+)?\.aliyuncs\.com)`},

	// ══════ AWS ══════
	{ID: "aws-akid", Category: "AWS·AccessKeyId", KeyName: "AccessKeyId", Level: "高危", ValueGroup: 1,
		Pattern: `(AKIA[0-9A-Z]{16})`},
	{ID: "aws-secretkey", Category: "AWS·SecretKey", KeyName: "SecretAccessKey", Level: "高危", ValueGroup: 1,
		Pattern: `(?i)["']?aws[_-]?secret[_-]?(?:access[_-]?)?key["']?\s*[:=]\s*["']([a-zA-Z0-9+/]{40})["']`},

	// ══════ 七牛云 ══════
	{ID: "qiniu-ak", Category: "七牛云·AccessKey", KeyName: "AccessKey", Level: "高危", ValueGroup: 1,
		Pattern: `(?i)["']?(?:qiniu[_-]?)?access[_-]?key["']?\s*[:=]\s*["']([a-zA-Z0-9_\-]{40,60})["']`},
	{ID: "qiniu-sk", Category: "七牛云·SecretKey", KeyName: "SecretKey", Level: "高危", ValueGroup: 1,
		Pattern: `(?i)["']?(?:qiniu[_-]?)?secret[_-]?key["']?\s*[:=]\s*["']([a-zA-Z0-9_\-]{40,60})["']`},

	// ══════ 华为云 ══════
	{ID: "hw-ak", Category: "华为云·AccessKey", KeyName: "HWAccessKey", Level: "高危", ValueGroup: 1,
		Pattern: `(?i)["']?hw[_-]?access[_-]?key["']?\s*[:=]\s*["']([A-Z0-9]{20})["']`},

	// ══════ 数据库 ══════
	{ID: "db-mongodb", Category: "数据库·MongoDB", KeyName: "MongoDB URL", Level: "高危", ValueGroup: 1,
		Pattern: `(mongodb(?:\+srv)?://[^\s"'<>\]]{10,})`},
	{ID: "db-mysql", Category: "数据库·MySQL", KeyName: "MySQL URL", Level: "高危", ValueGroup: 1,
		Pattern: `(mysql://[^\s"'<>\]]{10,})`},
	{ID: "db-redis", Category: "数据库·Redis", KeyName: "Redis URL", Level: "中危", ValueGroup: 1,
		Pattern: `(redis://[^\s"'<>\]]{6,})`},

	// ══════ 通用 ══════
	{ID: "gen-apikey", Category: "通用·APIKey", KeyName: "ApiKey", Level: "中危", ValueGroup: 1,
		Pattern: `(?i)["']?api[_-]?key["']?\s*[:=]\s*["']([a-zA-Z0-9_\-]{16,64})["']`},
	{ID: "gen-token", Category: "通用·Token", KeyName: "Token/AuthToken", Level: "中危", ValueGroup: 1,
		Pattern: `(?i)["']?(?:auth[_-]?)?token["']?\s*[:=]\s*["']([a-zA-Z0-9_\-\.]{24,128})["']`},
	{ID: "gen-password", Category: "通用·密码", KeyName: "Password", Level: "中危", ValueGroup: 1,
		Pattern: `(?i)["']?(?:password|passwd|pwd)["']?\s*[:=]\s*["']([^"']{6,32})["']`},
	{ID: "net-privateip", Category: "服务器·内网IP", KeyName: "PrivateIP", Level: "低危", ValueGroup: 1,
		Pattern: `["'/]((?:192\.168|10\.\d{1,3}|172\.(?:1[6-9]|2\d|3[01]))\.\d{1,3}\.\d{1,3})(?::\d+)?["'/]`},
}

// defaultRuleSet 返回内置规则的副本，并补齐 Enabled/Builtin 标记。
func defaultRuleSet() []Rule {
	out := make([]Rule, len(DefaultRules))
	for i, r := range DefaultRules {
		r.Enabled = true
		r.Builtin = true
		out[i] = r
	}
	return out
}

// SetRulesFilePath 指定 rules.json 位置（GUI 启动时设为 exe 同目录）。
func SetRulesFilePath(p string) {
	rulesMu.Lock()
	rulesFilePath = p
	rulesMu.Unlock()
}

// RulesFilePath 返回 rules.json 路径。
func RulesFilePath() string {
	rulesMu.RLock()
	p := rulesFilePath
	rulesMu.RUnlock()
	if p != "" {
		return p
	}
	exe, err := os.Executable()
	if err != nil {
		return "rules.json"
	}
	return filepath.Join(filepath.Dir(exe), "rules.json")
}

// LoadRules 从 rules.json 载入规则并编译。
// 文件不存在时用内置规则并写出一份，方便用户直接看到 / 手工编辑。
// 文件损坏时回退到内置规则，保证工具始终可用。
func LoadRules() []Rule {
	path := RulesFilePath()

	var rules []Rule
	data, err := os.ReadFile(path)
	if err == nil && len(data) > 0 {
		if jsonErr := json.Unmarshal(data, &rules); jsonErr != nil || len(rules) == 0 {
			rules = defaultRuleSet()
		} else {
			// 补齐 DefaultRules 中本地文件还没有的新内置规则（版本升级场景）
			rules = mergeBuiltin(rules)
		}
	} else {
		rules = defaultRuleSet()
		_ = writeRules(rules, path)
	}

	rulesMu.Lock()
	compiledCache = compileRules(rules)
	rulesLoaded = true
	rulesMu.Unlock()
	return rules
}

// mergeBuiltin 把 DefaultRules 里 rules.json 中缺失的内置规则补进来。
// 用户主动删掉的内置规则不会被悄悄恢复——删除时会在文件里留下 enabled=false 的记录。
func mergeBuiltin(user []Rule) []Rule {
	have := make(map[string]bool, len(user))
	for _, r := range user {
		have[r.ID] = true
	}
	for _, b := range defaultRuleSet() {
		if !have[b.ID] {
			user = append(user, b)
		}
	}
	return user
}

// SaveRules 校验并保存规则，然后立即生效（重新编译）。
// 任一规则非法则整体不保存，返回该规则的错误。
func SaveRules(rules []Rule) error {
	for i := range rules {
		if err := ValidateRule(rules[i]); err != nil {
			return fmt.Errorf("规则 [%s] %s", rules[i].Category, err)
		}
	}
	path := RulesFilePath()
	if err := writeRules(rules, path); err != nil {
		return err
	}
	rulesMu.Lock()
	compiledCache = compileRules(rules)
	rulesLoaded = true
	rulesMu.Unlock()
	return nil
}

// writeRules 原子写入（先写 .tmp 再 rename），避免中途失败留下半个文件。
func writeRules(rules []Rule, path string) error {
	data, err := json.MarshalIndent(rules, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

// ResetRules 恢复为内置规则集并保存。
func ResetRules() ([]Rule, error) {
	rules := defaultRuleSet()
	if err := SaveRules(rules); err != nil {
		return nil, err
	}
	return rules, nil
}

// ValidateRule 校验单条规则，错误信息直接面向用户展示。
func ValidateRule(r Rule) error {
	if strings.TrimSpace(r.Category) == "" {
		return fmt.Errorf("分类不能为空")
	}
	if strings.TrimSpace(r.KeyName) == "" {
		return fmt.Errorf("键名不能为空")
	}
	switch r.Level {
	case "高危", "中危", "低危":
	default:
		return fmt.Errorf("风险等级必须是 高危 / 中危 / 低危")
	}
	if strings.TrimSpace(r.Pattern) == "" {
		return fmt.Errorf("正则不能为空")
	}
	re, err := regexp.Compile(r.Pattern)
	if err != nil {
		// Go 用的是 RE2，不支持 (?=...) / (?!...) 之类的环视，这里给出明确提示
		msg := err.Error()
		if strings.Contains(msg, "invalid or unsupported Perl syntax") {
			return fmt.Errorf("正则语法不支持：Go 使用 RE2 引擎，不支持 (?=...)、(?!...) 等环视写法")
		}
		return fmt.Errorf("正则编译失败：%s", msg)
	}
	if r.ValueGroup < 0 {
		return fmt.Errorf("捕获组序号不能为负数")
	}
	if r.ValueGroup > re.NumSubexp() {
		return fmt.Errorf("捕获组序号 %d 超出正则的实际组数 %d", r.ValueGroup, re.NumSubexp())
	}
	return nil
}

// TestRule 用给定规则匹配一段文本，返回取到的值，供 UI 的"测试"功能使用。
func TestRule(r Rule, sample string) (string, error) {
	if err := ValidateRule(r); err != nil {
		return "", err
	}
	re := regexp.MustCompile(r.Pattern)
	m := re.FindStringSubmatch(sample)
	if m == nil {
		return "", fmt.Errorf("未匹配")
	}
	if r.ValueGroup == 0 || r.ValueGroup >= len(m) {
		return m[0], nil
	}
	return m[r.ValueGroup], nil
}

// compileRules 编译启用的规则，跳过编译失败的条目（避免一条坏规则让扫描完全瘫痪）。
func compileRules(rules []Rule) []compiledRule {
	out := make([]compiledRule, 0, len(rules))
	for _, r := range rules {
		if !r.Enabled {
			continue
		}
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			continue
		}
		out = append(out, compiledRule{Rule: r, regex: re})
	}
	return out
}

// activeRules 返回当前生效的规则快照，供 scanner 使用。
// 未调用过 LoadRules 时（例如命令行模式）退化为内置规则。
func activeRules() []compiledRule {
	rulesMu.RLock()
	if rulesLoaded {
		out := compiledCache
		rulesMu.RUnlock()
		return out
	}
	rulesMu.RUnlock()

	rulesMu.Lock()
	defer rulesMu.Unlock()
	if !rulesLoaded {
		compiledCache = compileRules(defaultRuleSet())
		rulesLoaded = true
	}
	return compiledCache
}

// CurrentRules 读取磁盘上的规则列表，供「规则管理」界面展示。
func CurrentRules() []Rule {
	path := RulesFilePath()
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return defaultRuleSet()
	}
	var rules []Rule
	if err := json.Unmarshal(data, &rules); err != nil || len(rules) == 0 {
		return defaultRuleSet()
	}
	return mergeBuiltin(rules)
}

// LevelFromString 把等级字符串转成枚举。
func LevelFromString(s string) SensitiveLevel {
	switch s {
	case "高危":
		return LevelHigh
	case "低危":
		return LevelLow
	default:
		return LevelMedium
	}
}
