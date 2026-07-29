package gui

import (
	"syscall"
	"unsafe"
)

// DWMWA_CLOAK = 13，Win8+ 支持。
// 被 cloak 的窗口对 Windows 而言仍然是"可见"的（WM_SIZE / 布局 / 绘制都照常进行），
// 只是不会被 DWM 合成到屏幕上。这正好用来消除启动白屏：
// 先 cloak → 显示并完成布局与首次绘制 → 再 uncloak，用户看到的第一帧就是完整界面。
//
// 之所以不能用 walk 的 Visible:false 达到同样目的：
// walk 的 WindowBase.RequestLayout() 在窗口不可见时会直接 return，
// 导致 Create 阶段所有子控件的布局请求被丢弃，窗口会永久空白。
const dwmwaCloak = 13

var (
	dwmapi                    = syscall.NewLazyDLL("dwmapi.dll")
	procDwmSetWindowAttribute = dwmapi.NewProc("DwmSetWindowAttribute")
)

// setWindowCloak 开启/关闭窗口的 DWM cloak，返回是否真正生效。
// 返回 false 表示系统不支持（低于 Win8、dwmapi 缺失或桌面合成未开启），
// 调用方必须退回"直接 Show"的老路径：顶多闪一下，绝不能把窗口留在隐身状态。
func setWindowCloak(hwnd uintptr, cloak bool) bool {
	if err := procDwmSetWindowAttribute.Find(); err != nil {
		return false
	}
	var v int32
	if cloak {
		v = 1
	}
	// DwmSetWindowAttribute 返回 HRESULT，只有 S_OK(0) 才算成功
	hr, _, _ := procDwmSetWindowAttribute.Call(
		hwnd,
		uintptr(dwmwaCloak),
		uintptr(unsafe.Pointer(&v)),
		unsafe.Sizeof(v),
	)
	return hr == 0
}
