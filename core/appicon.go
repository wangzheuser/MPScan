package core

import (
	"os"
	"path/filepath"
	"strings"
)

// 小程序图标的真实来源。
//
// 微信 PC 端（xwechat）把小程序包和图标分开放在同一层：
//
//	.../applet/package/<wxid>/            小程序包（这就是工具的"监控目录"）
//	.../applet/ico/<wxid>_<hash>.png      对应的官方图标
//
// 例：
//
//	package/wx608c36ee909921b5
//	  ↔ ico/wx608c36ee909921b5_5ef5e68352f011a030e6f575d8266636.png
//
// 所以图标可以从监控目录用相对路径推出来，不需要联网、不需要读微信数据库。
// 文件名前缀就是 wxid，后缀是哈希/版本号，因此按 "<wxid>_" 前缀匹配即可。

// iconDirNames 图标目录的候选名。
// 不同版本大小写/单复数可能有出入，按顺序探测；
// 同时监控目录本身可能被用户指向 package 或 applet 任一层，两种都覆盖。
var iconDirNames = []string{"ico", "icon", "icons"}

// FindAppIcon 根据监控目录和 wxid 找出官方图标的绝对路径。
// 找不到返回空字符串——调用方据此安静降级，不显示图标。
//
// watchDir 通常是 .../applet/package，图标在其兄弟目录 .../applet/ico。
// 为兼容用户把监控目录直接指到 applet 层的情况，也会在 watchDir 内部找一次。
func FindAppIcon(watchDir, wxid string) string {
	if watchDir == "" || wxid == "" {
		return ""
	}

	for _, dir := range candidateIconDirs(watchDir) {
		if p := matchIconInDir(dir, wxid); p != "" {
			return p
		}
	}
	return ""
}

// IconCandidateDirs 暴露候选目录，供界面在日志里说明"到哪儿找过了"。
// 图标没出来时，光看"没有图标"无法判断是目录不对还是文件名不对。
func IconCandidateDirs(watchDir string) []string {
	if watchDir == "" {
		return nil
	}
	return candidateIconDirs(watchDir)
}

// iconSearchDepth 向上回溯的层数。
//
// 正常情况 watchDir = .../applet/package，ico 就在上一层；
// 但用户也可能把目录指到 .../applet、.../applet/package/wxXXX，
// 甚至 users/<hash>，所以从监控目录起向上找几层，每层都试一次。
// 4 层足够覆盖 users/<hash>/applet/package/<wxid>，又不会跑到磁盘根上乱翻。
const iconSearchDepth = 4

// candidateIconDirs 列出可能存放图标的目录，按可能性排序。
//
// 先看监控目录内部，再逐层往上找同名目录：越靠近监控目录的越可能是对的。
func candidateIconDirs(watchDir string) []string {
	clean := filepath.Clean(watchDir)

	dirs := make([]string, 0, len(iconDirNames)*(iconSearchDepth+1)*2)
	cur := clean
	for i := 0; i <= iconSearchDepth; i++ {
		for _, name := range iconDirNames {
			dirs = append(dirs, filepath.Join(cur, name))
			// 目录指到了 applet 的上一层（如 users/<hash>）时，
			// ico 在 applet 里面，往下钻一层也要试。
			dirs = append(dirs, filepath.Join(cur, "applet", name))
		}
		parent := filepath.Dir(cur)
		if parent == cur { // 到根了
			break
		}
		cur = parent
	}
	return dirs
}

// matchIconInDir 在一个目录里找 "<wxid>_*" 的图片文件。
// 命名格式是 wx{AppID}_{hash}.png；同一个 wxid 可能残留多个历史版本，
// 取修改时间最新的那个，保证跟当前缓存的小程序版本一致。
func matchIconInDir(dir, wxid string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}

	prefix := wxid + "_"
	var best string
	var bestMod int64 = -1

	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasPrefix(name, prefix) || !isImageName(name) {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			// 拿不到时间也别丢弃，作为最低优先级候选
			if best == "" {
				best = filepath.Join(dir, name)
			}
			continue
		}
		if mod := info.ModTime().UnixNano(); mod > bestMod {
			bestMod = mod
			best = filepath.Join(dir, name)
		}
	}
	return best
}

// isImageName 判断是否是能解码的图片扩展名。
func isImageName(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png", ".jpg", ".jpeg":
		return true
	}
	return false
}
