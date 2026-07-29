package core

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SensitiveLevel 敏感信息风险等级
type SensitiveLevel string

const (
	LevelHigh   SensitiveLevel = "高危"
	LevelMedium SensitiveLevel = "中危"
	LevelLow    SensitiveLevel = "低危"
)

// ScanResult 单条敏感信息扫描结果
type ScanResult struct {
	WxID     string
	AppName  string
	Category string
	Level    SensitiveLevel
	KeyName  string
	Value    string
	FilePath string
	LineNo   int
}

// 敏感信息匹配规则已抽到 rules.go，支持在 UI 的「规则管理」中修改和新增，
// 内置默认规则见 DefaultRules，运行时生效的规则通过 activeRules() 获取。

// 要扫描的文件后缀
var scanExtensions = map[string]bool{
	".js": true, ".json": true, ".ts": true, ".wxs": true,
}

const (
	// 反编译出来的 app-service.js 动辄十几 MB，旧的 2MB 上限会把这些文件整个跳过，
	// 而 AK/SK 这类东西恰好最常出现在里面，所以把上限抬到 64MB 并在跳过时记日志。
	maxFileSize = 64 * 1024 * 1024

	// 单行、单条规则最多取的匹配数。压缩后的 JS 一行可能有几 MB，
	// 全量取匹配在极端情况下会产生大量重复结果，这里留个上限。
	maxMatchesPerLine = 500
)

// ScanDecompiledDir 扫描反编译目录，提取敏感信息
func ScanDecompiledDir(wxid, appName, decompiledDir string, logFunc func(string)) []*ScanResult {
	if appName == "" {
		appName = wxid
	}
	logFunc(fmt.Sprintf("[扫描] 开始提取敏感信息 → %s", appName))

	seen := make(map[string]bool)
	var results []*ScanResult

	_ = filepath.Walk(decompiledDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}
		if !scanExtensions[strings.ToLower(filepath.Ext(path))] {
			return nil
		}
		if info.Size() > maxFileSize {
			// 不再静默跳过，否则漏报时完全没有线索
			logFunc(fmt.Sprintf("[!] 文件过大已跳过（%.1fMB > %dMB）：%s",
				float64(info.Size())/(1024*1024), maxFileSize/(1024*1024), path))
			return nil
		}
		results = append(results, scanFile(path, wxid, appName, seen)...)
		return nil
	})

	high, mid, low := countLevels(results)
	logFunc(fmt.Sprintf("[扫描] %s 完成 → 高危%d 中危%d 低危%d，共%d条", appName, high, mid, low, len(results)))
	return results
}

func countLevels(results []*ScanResult) (high, mid, low int) {
	for _, r := range results {
		switch r.Level {
		case LevelHigh:
			high++
		case LevelMedium:
			mid++
		case LevelLow:
			low++
		}
	}
	return
}

func scanFile(path, wxid, appName string, seen map[string]bool) []*ScanResult {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	// 取一次规则快照，避免扫描过程中用户保存规则导致中途换表
	rules := activeRules()

	var results []*ScanResult

	// 用 bufio.Reader 逐行读，不能用 bufio.Scanner：Scanner 的单行上限（之前是 1MB）
	// 一旦被压缩成一整行的 JS 顶破，Scan() 会直接返回 false，文件剩下的部分全部不扫，
	// 而且不报错。ReadString 没有这个限制。
	rd := bufio.NewReaderSize(f, 1<<20)

	lineNo := 0
	for {
		line, err := rd.ReadString('\n')
		if line == "" && err != nil {
			break
		}
		lineNo++
		line = strings.TrimRight(line, "\r\n")

		for _, pat := range rules {
			// 一行里可能有多组 AK/SK（压缩后的 JS 尤其常见），
			// 只取第一个匹配会漏掉后面的，所以取全部匹配。
			for _, matches := range pat.regex.FindAllStringSubmatch(line, maxMatchesPerLine) {
				var value string
				if pat.ValueGroup == 0 || pat.ValueGroup >= len(matches) {
					value = matches[0]
				} else {
					value = matches[pat.ValueGroup]
				}
				if value == "" {
					continue
				}

				key := pat.Category + ":" + value
				if seen[key] {
					continue
				}
				seen[key] = true

				display := value
				if len(display) > 80 {
					display = display[:77] + "..."
				}

				results = append(results, &ScanResult{
					WxID:     wxid,
					AppName:  appName,
					Category: pat.Category,
					Level:    LevelFromString(pat.Level),
					KeyName:  pat.KeyName,
					Value:    display,
					FilePath: path,
					LineNo:   lineNo,
				})
			}
		}

		if err != nil {
			break
		}
	}
	return results
}

// ResolveNameFromDecompiledDir 从反编译输出目录中读取真实小程序名称。
// 优先读取 app-config.json → global.window / window → navigationBarTitleText；
// 再读 project.config.json → "projectname"；
// 最后回退读取 app.json → "window" → "navigationBarTitleText"。
func ResolveNameFromDecompiledDir(decompiledDir string) string {
	// 策略1: app-config.json（与 Python 脚本逻辑一致，优先级最高）
	if name := readAppConfigJsonTitle(decompiledDir); name != "" {
		return name
	}
	// 策略2: project.config.json
	if name := readProjectConfigName(decompiledDir); name != "" {
		return name
	}
	// 策略3: app.json window.navigationBarTitleText
	if name := readAppJsonTitle(decompiledDir); name != "" {
		return name
	}
	return ""
}

// readAppConfigJsonTitle 查找 app-config.json，读取 global.window.navigationBarTitleText
// 或 window.navigationBarTitleText（与 Python 版 find_miniapp_names.py 逻辑一致）。
func readAppConfigJsonTitle(root string) string {
	var candidates []string
	// 递归 Walk 查找所有 app-config.json
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && info.Name() == "app-config.json" {
			candidates = append(candidates, path)
		}
		return nil
	})
	for _, p := range candidates {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var obj map[string]interface{}
		if err := json.Unmarshal(data, &obj); err != nil {
			continue
		}
		// 优先：global.window.navigationBarTitleText
		if globalObj, ok := obj["global"].(map[string]interface{}); ok {
			if winObj, ok := globalObj["window"].(map[string]interface{}); ok {
				if v, ok := winObj["navigationBarTitleText"]; ok {
					if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
						return strings.TrimSpace(s)
					}
				}
			}
		}
		// 回退：window.navigationBarTitleText
		if winObj, ok := obj["window"].(map[string]interface{}); ok {
			if v, ok := winObj["navigationBarTitleText"]; ok {
				if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
					return strings.TrimSpace(s)
				}
			}
		}
	}
	return ""
}

func readProjectConfigName(root string) string {
	candidates := []string{
		filepath.Join(root, "project.config.json"),
	}
	// 也搜索一级子目录（反编译后有时会放在子文件夹里）
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if e.IsDir() {
			candidates = append(candidates, filepath.Join(root, e.Name(), "project.config.json"))
		}
	}
	for _, p := range candidates {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var obj map[string]interface{}
		if err := json.Unmarshal(data, &obj); err != nil {
			continue
		}
		if v, ok := obj["projectname"]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

func readAppJsonTitle(root string) string {
	candidates := []string{
		filepath.Join(root, "app.json"),
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if e.IsDir() {
			candidates = append(candidates, filepath.Join(root, e.Name(), "app.json"))
		}
	}
	for _, p := range candidates {
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var obj map[string]interface{}
		if err := json.Unmarshal(data, &obj); err != nil {
			continue
		}
		if winObj, ok := obj["window"].(map[string]interface{}); ok {
			if v, ok := winObj["navigationBarTitleText"]; ok {
				if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
					return strings.TrimSpace(s)
				}
			}
		}
	}
	return ""
}
