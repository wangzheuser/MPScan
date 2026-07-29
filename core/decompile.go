package core

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha1"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/wux1an/wxapkg/util"
	"golang.org/x/crypto/pbkdf2"
)

// DecompileResult 反编译结果
type DecompileResult struct {
	WxID      string
	InputPath string
	OutputDir string
	FileCount int
	Error     error
}

// DecompileOptions 反编译选项
type DecompileOptions struct {
	Thread          int
	DisableBeautify bool
}

// DefaultOptions 默认选项
var DefaultOptions = DecompileOptions{
	Thread:          30,
	DisableBeautify: false,
}

// DecompileDir 对整个小程序目录（wx开头的文件夹）进行反编译
// root: 形如 D:\WeChat Files\Applet\wx1234567890abcdef
// outputBase: 输出的根目录，会在该目录下创建以wxid命名的子目录
//
// 微信小程序目录的常规结构：
//
//	wxXXX/
//	  ├─ 0/   (主包 .wxapkg)
//	  ├─ 1/   (分包 .wxapkg)
//	  └─ ...
//
// 主包与各分包会被统一反编译到同一个 outputDir 根下，因为每个 wxapkg
// 内部记录的文件路径已经天然区分（主包的 app.json / 分包的 pages 等），
// 合并后即可还原一个完整的小程序文件结构，便于后续扫描与人工分析。
func DecompileDir(root, outputBase string, opts DecompileOptions, logFunc func(string)) (*DecompileResult, error) {
	wxid, err := parseWxid(root)
	if err != nil {
		return nil, err
	}

	dirs, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}

	outputDir := filepath.Join(outputBase, wxid)
	result := &DecompileResult{
		WxID:      wxid,
		InputPath: root,
		OutputDir: outputDir,
	}

	logFunc(fmt.Sprintf("[+] 开始反编译 '%s'，使用 %d 个线程", filepath.Base(root), opts.Thread))

	// 收集所有 wxapkg 文件（包括主包与分包），统一输出到 outputDir
	type packInfo struct {
		path string
		// isMain 通过目录号推断主包（数字越小越靠前，"0" 视为主包）
		isMain bool
	}
	var packs []packInfo
	for _, subDir := range dirs {
		if !subDir.IsDir() {
			continue
		}
		files, ferr := scanFiles(filepath.Join(root, subDir.Name()))
		if ferr != nil {
			logFunc(fmt.Sprintf("[-] 跳过 '%s': %v", subDir.Name(), ferr))
			continue
		}
		isMain := subDir.Name() == "0" || subDir.Name() == "main"
		for _, f := range files {
			packs = append(packs, packInfo{path: f, isMain: isMain})
		}
	}

	logFunc(fmt.Sprintf("[+] 共发现 %d 个 wxapkg 文件（主包+分包），统一输出到 %s", len(packs), outputDir))

	var allFileCount int
	for _, p := range packs {
		decryptedData, derr := decryptFileSafe(wxid, p.path)
		if derr != nil {
			logFunc(fmt.Sprintf("[!] 解密失败 '%s': %v", filepath.Base(p.path), derr))
			continue
		}
		fileCount, uerr := unpack(decryptedData, outputDir, opts.Thread, !opts.DisableBeautify)
		if uerr != nil {
			// 部分分包文件在解密后可能不含 0xBE/0xED 头部（例如独立分包或增量更新包），
			// 这里给出更明确的提示而不是简单的"格式无效"。
			logFunc(fmt.Sprintf("[!] 解包失败 '%s'（可能是非标准分包/增量包）: %v", filepath.Base(p.path), uerr))
			continue
		}
		allFileCount += fileCount
		rel, _ := filepath.Rel(filepath.Dir(root), p.path)
		tag := "分包"
		if p.isMain {
			tag = "主包"
		}
		logFunc(fmt.Sprintf("[+] 已解包 %s %d 个文件 <- '%s'", tag, fileCount, rel))
	}

	result.FileCount = allFileCount
	logFunc(fmt.Sprintf("[✓] 全部完成！共 %d 个文件 -> '%s'", allFileCount, outputDir))
	return result, nil
}

// DecompileSingleFile 对单个 wxapkg 文件进行反编译
// wxapkgPath: 单个wxapkg文件路径（需要能从路径中解析出wxid）
// outputDir: 输出目录
func DecompileSingleFile(wxapkgPath, outputDir string, opts DecompileOptions, logFunc func(string)) (*DecompileResult, error) {
	// 尝试从文件所在目录的父目录解析wxid
	wxid, err := parseWxidFromFile(wxapkgPath)
	if err != nil {
		return nil, err
	}

	decryptedData, err := decryptFileSafe(wxid, wxapkgPath)
	if err != nil {
		return nil, err
	}
	// 单文件反编译也直接输出到 wxid 根目录，与 DecompileDir 行为一致，
	// 这样后续扫描 / 小程序名称解析 都可走统一逻辑。
	subOutputDir := filepath.Join(outputDir, wxid)
	fileCount, err := unpack(decryptedData, subOutputDir, opts.Thread, !opts.DisableBeautify)
	if err != nil {
		return nil, err
	}

	logFunc(fmt.Sprintf("[✓] 反编译完成: %d 个文件 -> '%s'", fileCount, subOutputDir))
	return &DecompileResult{
		WxID:      wxid,
		InputPath: wxapkgPath,
		OutputDir: subOutputDir,
		FileCount: fileCount,
	}, nil
}

// parseWxid 从目录名中解析 wxid（格式如 wx1234567890abcdef）
func parseWxid(root string) (string, error) {
	var regAppId = regexp.MustCompile(`(wx[0-9a-f]{16})`)
	base := filepath.Base(root)
	if !regAppId.MatchString(base) {
		return "", errors.New("路径中未找到有效的小程序ID（wx开头的16位ID）")
	}
	return regAppId.FindStringSubmatch(base)[1], nil
}

// parseWxidFromFile 从 wxapkg 文件路径向上查找 wxid
func parseWxidFromFile(wxapkgPath string) (string, error) {
	var regAppId = regexp.MustCompile(`(wx[0-9a-f]{16})`)
	path := wxapkgPath
	for i := 0; i < 5; i++ {
		if regAppId.MatchString(filepath.Base(path)) {
			return regAppId.FindStringSubmatch(filepath.Base(path))[1], nil
		}
		path = filepath.Dir(path)
	}
	return "", errors.New("无法从路径中解析小程序ID，请确保路径包含 wx 开头的16位ID")
}

// scanFiles 在指定目录中递归查找所有 .wxapkg 文件
func scanFiles(root string) ([]string, error) {
	paths, err := util.GetDirAllFilePaths(root, "", ".wxapkg")
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("'%s' 中未找到 .wxapkg 文件", root)
	}
	return paths, nil
}

// decryptFileSafe 解密 wxapkg 文件，返回错误而非 log.Fatal，
// 这样一个坏文件不会拖垮整个反编译流程。
func decryptFileSafe(wxid, wxapkgPath string) ([]byte, error) {
	var (
		salt = "saltiest"
		iv   = "the iv: 16 bytes"
	)

	dataByte, err := os.ReadFile(wxapkgPath)
	if err != nil {
		return nil, err
	}
	// wxapkg 头部至少需要 6 + 1024 字节用于 AES-CBC 解密
	if len(dataByte) < 1024+6 {
		return nil, fmt.Errorf("文件过小（%d 字节），可能不是有效 wxapkg 或下载未完成", len(dataByte))
	}

	dk := pbkdf2.Key([]byte(wxid), []byte(salt), 1000, 32, sha1.New)
	block, _ := aes.NewCipher(dk)
	blockMode := cipher.NewCBCDecrypter(block, []byte(iv))
	originData := make([]byte, 1024)
	blockMode.CryptBlocks(originData, dataByte[6:1024+6])

	afData := make([]byte, len(dataByte)-1024-6)
	var xorKey = byte(0x66)
	if len(wxid) >= 2 {
		xorKey = wxid[len(wxid)-2]
	}
	for i, b := range dataByte[1024+6:] {
		afData[i] = b ^ xorKey
	}

	originData = append(originData[:1023], afData...)
	return originData, nil
}

type wxapkgFile struct {
	nameLen uint32
	name    []byte
	offset  uint32
	size    uint32
}

var exts = make(map[string]int)
var extsLocker = sync.Mutex{}
var beautifyFuncs = map[string]func([]byte) []byte{
	".json": util.PrettyJson,
	".html": util.PrettyHtml,
	".js":   util.PrettyJavaScript,
}

// unpack 解包解密后的数据到指定目录
func unpack(decryptedData []byte, unpackRoot string, thread int, doBeautify bool) (int, error) {
	var f = bytes.NewReader(decryptedData)

	var (
		firstMark       uint8
		info1           uint32
		indexInfoLength uint32
		bodyInfoLength  uint32
		lastMark        uint8
	)
	_ = binary.Read(f, binary.BigEndian, &firstMark)
	_ = binary.Read(f, binary.BigEndian, &info1)
	_ = binary.Read(f, binary.BigEndian, &indexInfoLength)
	_ = binary.Read(f, binary.BigEndian, &bodyInfoLength)
	_ = binary.Read(f, binary.BigEndian, &lastMark)

	if firstMark != 0xBE || lastMark != 0xED {
		return 0, errors.New("解包失败：不是有效的 wxapkg 文件格式")
	}

	var fileCount uint32
	_ = binary.Read(f, binary.BigEndian, &fileCount)

	var fileList = make([]*wxapkgFile, fileCount)
	for i := uint32(0); i < fileCount; i++ {
		data := &wxapkgFile{}
		_ = binary.Read(f, binary.BigEndian, &data.nameLen)

		if data.nameLen > 10<<20 {
			return 0, errors.New("解密数据无效：文件名长度异常")
		}

		data.name = make([]byte, data.nameLen)
		_, _ = io.ReadAtLeast(f, data.name, int(data.nameLen))
		_ = binary.Read(f, binary.BigEndian, &data.offset)
		_ = binary.Read(f, binary.BigEndian, &data.size)

		fileList[i] = data
	}

	var chFiles = make(chan *wxapkgFile)
	var wg = sync.WaitGroup{}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for _, d := range fileList {
			chFiles <- d
		}
		close(chFiles)
	}()

	wg.Add(thread)
	var locker = sync.Mutex{}
	var count = 0
	for i := 0; i < thread; i++ {
		go func() {
			defer wg.Done()
			for d := range chFiles {
				// 规范化路径分隔符
				namePath := strings.ReplaceAll(string(d.name), "/", string(filepath.Separator))
				outputFilePath := filepath.Join(unpackRoot, namePath)
				dir := filepath.Dir(outputFilePath)

				err := os.MkdirAll(dir, os.ModePerm)
				if err != nil {
					continue
				}

				data := decryptedData[d.offset : d.offset+d.size]

				if doBeautify {
					data = fileBeautify(outputFilePath, data)
				}
				_ = os.WriteFile(outputFilePath, data, 0600)

				locker.Lock()
				count++
				locker.Unlock()
			}
		}()
	}

	wg.Wait()
	return int(fileCount), nil
}

func fileBeautify(name string, data []byte) (result []byte) {
	defer func() {
		if err := recover(); err != nil {
			result = data
		}
	}()

	var ext = filepath.Ext(name)
	extsLocker.Lock()
	exts[ext] = exts[ext] + 1
	extsLocker.Unlock()

	b, ok := beautifyFuncs[ext]
	if !ok {
		return data
	}
	return b(data)
}
