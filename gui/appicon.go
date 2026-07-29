package gui

import (
	"fmt"
	"image"
	"image/draw"
	_ "image/jpeg" // 注册 JPEG 解码器
	_ "image/png"  // 注册 PNG 解码器
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/lxn/walk"
	"github.com/lxn/win"
	"github.com/wux1an/wxapkg/core"
)

// 结果表第一列的小程序图标。
//
// 图标来自微信自己的缓存（见 core.FindAppIcon），是官方图标而不是从包里猜的图片。
// 主要作用是在小程序真实名称没能解析出来（列里只剩 wxid）时还能确认资产归属。
//
// 三个必须注意的 walk 约束：
//
//  1. 图标必须按 TableView 的真实 DPI 构造，否则根本画不出来。
//     walk 取图标句柄走 Icon.handleForDPI(dpi)，它只在 dpi2hIcon[dpi] 里找；
//     而 walk.NewIconFromImage 内部固定按 96dpi 注册（icon.go:155）。
//     150% 缩放（144dpi）下查 dpi2hIcon[144] 落空，又因为图像构造的 Icon
//     没有 filePath/res/isStock 可回退加载，handleForDPI 直接返回 0，
//     ImageList_ReplaceIcon(hIml, -1, 0) 返回 -1，于是整列图标全都不显示。
//     实测：NewIconFromImage 的图标在 dpi=144 下 ReplaceIcon 返回 -1，
//     用 NewIconFromImageForDPI(im, 144) 构造则返回有效索引。
//     所以这里用 NewIconFromImageForDPI，并按 dpi 分别缓存。
//
//  2. walk 的 ImageList 按 GetSystemMetricsForDpi(SM_CXSMICON, dpi) 建立且不缩放
//     （imagelist.go:184），所以要先把图片缩到该 DPI 对应的尺寸再交给它。
//     注意 GetSystemMetrics（不带 ForDpi）在 DPI 感知进程里返回的是已缩放值，
//     在 go test 里却是 96dpi 的值，两者不能混用。
//
//  3. 用 *walk.Icon 而不是 *walk.Bitmap：Bitmap 走 ImageList_AddMasked(hBmp, 0)，
//     会把黑色当成透明掩码，图标里的黑色区域会被打穿。
//
// 图标对象在 TableView 引用期间必须一直存活，所以缓存只增不减（数量等于小程序数，可控）。

// iconKey 同一个小程序在不同 DPI 下要各存一份图标句柄。
type iconKey struct {
	wxid string
	dpi  int
}

var (
	iconMu    sync.Mutex
	iconCache = map[iconKey]*walk.Icon{} // nil 表示查过但没有，避免反复读盘
	iconPath  = map[string]string{}      // wxid -> 图标文件路径；"" 表示查过没有
	iconDir   string                     // 当前监控目录，用于推算图标位置

	// blankIcon 全透明占位图标，给"没有对应图标"的行用（按 DPI 各存一份）。
	//
	// 统一返回一个同尺寸的透明图标而不是留空：视觉上是干净空白，
	// 且各行文字左边距一致，不会因为有的行有图标、有的行没有而参差不齐。
	blankIcon = map[int]*walk.Icon{}
)

// smallIconSizeForDPI 返回该 DPI 下的系统小图标尺寸。
// 必须和 walk 建 ImageList 用的算法一致（imagelist.go:184）。
func smallIconSizeForDPI(dpi int) (int, int) {
	w := int(win.GetSystemMetricsForDpi(win.SM_CXSMICON, uint32(dpi)))
	h := int(win.GetSystemMetricsForDpi(win.SM_CYSMICON, uint32(dpi)))
	if w <= 0 || h <= 0 {
		return 16, 16
	}
	return w, h
}

// blankPlaceholder 惰性构造该 DPI 的全透明占位图标；失败返回 nil。
// 调用方已持有 iconMu。
func blankPlaceholder(dpi int) *walk.Icon {
	if ic, ok := blankIcon[dpi]; ok {
		return ic
	}
	w, h := smallIconSizeForDPI(dpi)
	// image.NewRGBA 的零值就是全透明（A=0），不需要再填
	ic, err := walk.NewIconFromImageForDPI(image.NewRGBA(image.Rect(0, 0, w, h)), dpi)
	if err != nil {
		ic = nil
	}
	blankIcon[dpi] = ic
	return ic
}

// logIconSourceState 把图标目录的探测结果写进日志。
//
// 没有图标时，界面上「透明占位」和「真的没找到」看起来完全一样，
// 排查时无从下手。所以扫描开始前先说明：到哪些目录找过、各有多少候选图片。
func logIconSourceState(watchDir string, logf func(string)) {
	if logf == nil {
		return
	}

	found := false
	for _, dir := range core.IconCandidateDirs(watchDir) {
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // 目录不存在是常态，不逐个报噪音
		}
		n := 0
		for _, e := range entries {
			if !e.IsDir() && isIconFileName(e.Name()) {
				n++
			}
		}
		found = true
		logf(fmt.Sprintf("[i] 图标目录 '%s'：%d 个候选图片", dir, n))
	}
	if !found {
		logf(fmt.Sprintf("[!] 未找到图标目录（已在 '%s' 的同级与内部查找 ico/icon/icons），结果表第一列将留空",
			filepath.Clean(watchDir)))
	}
}

func isIconFileName(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".png", ".jpg", ".jpeg":
		return true
	}
	return false
}

// setIconSourceDir 记录监控目录。切换目录时清空缓存，
// 否则换了微信账号/目录后仍会显示上一批图标。
func setIconSourceDir(watchDir string) {
	iconMu.Lock()
	defer iconMu.Unlock()
	if iconDir == watchDir {
		return
	}
	iconDir = watchDir
	iconCache = map[iconKey]*walk.Icon{}
	iconPath = map[string]string{}
}

// warmIconForWxID 在扫描线程上预先解析并缓存图标，同时把结果写进日志。
//
// 不在 StyleCell 里记日志：StyleCell 跑在绘制路径上（LVN_GETDISPINFO），
// 而写日志会 SetText 日志框、再触发一轮重绘，等于在绘制里套绘制。
// 这里提前把图标准备好，StyleCell 之后只会命中缓存。
func warmIconForWxID(wxid string, logf func(string)) {
	if wxid == "" {
		return
	}

	iconMu.Lock()
	_, hit := iconPath[wxid]
	dir := iconDir
	iconMu.Unlock()
	if hit || dir == "" {
		return // 已经查过，或还不知道监控目录
	}

	path := core.FindAppIcon(dir, wxid)

	iconMu.Lock()
	if iconDir == dir { // 读盘期间目录可能被切换
		iconPath[wxid] = path
	}
	iconMu.Unlock()

	if logf == nil {
		return
	}
	if path == "" {
		logf(fmt.Sprintf("[!] %s 未找到对应图标，第一列留空", wxid))
	} else {
		logf(fmt.Sprintf("[i] %s 图标已加载：%s", wxid, filepath.Base(path)))
	}
}

// iconForWxID 取某个小程序在指定 DPI 下的图标。
// 没有对应图标时返回透明占位图标：保持空白，同时让每行左边距一致。
//
// dpi 必须是 TableView 当前的 DPI（style.DPI()）：walk 用同一个 dpi 去
// Icon.handleForDPI 取句柄，构造用的 DPI 不一致就会取到 0，导致整列不显示。
func iconForWxID(wxid string, dpi int) *walk.Icon {
	iconMu.Lock()
	defer iconMu.Unlock()

	if wxid == "" || iconDir == "" {
		return blankPlaceholder(dpi)
	}

	key := iconKey{wxid: wxid, dpi: dpi}
	if ic, hit := iconCache[key]; hit {
		if ic != nil {
			return ic
		}
		return blankPlaceholder(dpi)
	}

	// 路径由 warmIconForWxID 在扫描线程上预先查好；这里只在没预热到时兜底查一次
	path, known := iconPath[wxid]
	if !known {
		path = core.FindAppIcon(iconDir, wxid)
		iconPath[wxid] = path
	}

	ic := loadScaledIcon(path, dpi)
	iconCache[key] = ic // 失败也记 nil，避免每次重绘都读盘
	if ic != nil {
		return ic
	}
	return blankPlaceholder(dpi)
}

// loadScaledIcon 读图片、缩到该 DPI 的小图标尺寸、转成 walk.Icon。
// 任何一步失败都返回 nil。
//
// 必须用 NewIconFromImageForDPI 而不是 NewIconFromImage：后者固定按 96dpi
// 注册句柄，在 144dpi 的 TableView 上 handleForDPI 会返回 0，图标不显示。
func loadScaledIcon(path string, dpi int) *walk.Icon {
	if path == "" {
		return nil
	}

	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	src, _, err := image.Decode(f)
	if err != nil {
		return nil
	}

	w, h := smallIconSizeForDPI(dpi)
	scaled := scaleToSquare(src, w, h)

	ic, err := walk.NewIconFromImageForDPI(scaled, dpi)
	if err != nil {
		return nil
	}
	return ic
}

// scaleToSquare 把图片缩放到 w×h。
//
// 只用标准库（离线环境拉不到 golang.org/x/image），所以自己做盒式平均：
// 每个目标像素取源图对应矩形区域的平均值。相比最近邻，16px 下边缘不会有锯齿。
// 非正方形源图先按短边中心裁剪成正方形，避免图标被拉扁。
func scaleToSquare(src image.Image, w, h int) image.Image {
	b := src.Bounds()
	if b.Dx() <= 0 || b.Dy() <= 0 {
		return src
	}

	// 中心裁剪成正方形
	side := b.Dx()
	if b.Dy() < side {
		side = b.Dy()
	}
	crop := image.Rect(
		b.Min.X+(b.Dx()-side)/2,
		b.Min.Y+(b.Dy()-side)/2,
		b.Min.X+(b.Dx()-side)/2+side,
		b.Min.Y+(b.Dy()-side)/2+side,
	)

	// 统一转成 RGBA 便于按坐标取值
	srcRGBA := image.NewRGBA(image.Rect(0, 0, side, side))
	draw.Draw(srcRGBA, srcRGBA.Bounds(), src, crop.Min, draw.Src)

	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		y0 := y * side / h
		y1 := (y + 1) * side / h
		if y1 <= y0 {
			y1 = y0 + 1
		}
		for x := 0; x < w; x++ {
			x0 := x * side / w
			x1 := (x + 1) * side / w
			if x1 <= x0 {
				x1 = x0 + 1
			}

			var sr, sg, sb, sa, n uint32
			for sy := y0; sy < y1; sy++ {
				for sx := x0; sx < x1; sx++ {
					r, g, bl, a := srcRGBA.At(sx, sy).RGBA()
					sr += r
					sg += g
					sb += bl
					sa += a
					n++
				}
			}
			if n == 0 {
				continue
			}
			i := dst.PixOffset(x, y)
			dst.Pix[i+0] = uint8(sr / n >> 8)
			dst.Pix[i+1] = uint8(sg / n >> 8)
			dst.Pix[i+2] = uint8(sb / n >> 8)
			dst.Pix[i+3] = uint8(sa / n >> 8)
		}
	}
	return dst
}
