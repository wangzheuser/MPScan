package gui

import (
	"sync/atomic"
	"time"

	"github.com/lxn/walk"
	. "github.com/lxn/walk/declarative"
	"github.com/lxn/win"
)

// createTipWindow 创建无边框浮动提示窗口（红色加粗大字）。
//
// 关于启动时多出一个白窗口的问题：
// walk 的 declarative MainWindow.Create() 末尾有 `if mw.Visible != false { w.Show() }`，
// 所以这个提示窗口在创建时会被直接显示出来——此时边框还没去掉、CustomWidget 还没绘制，
// 用户看到的就是主界面之外的一个白框。
//
// 但也不能简单加个 Visible:false 就完事：walk 的布局是异步算的，
// 窗口从没显示过的话子控件不会被布局，之后 showTip 弹出来会是个空白框。
// 所以这里的做法是：先 cloak（DWM 不合成到屏幕）→ 显示并完成一次真实布局 → 隐藏 → uncloak。
// 整个过程用户看不到任何窗口，而 CustomWidget 已经拿到正确尺寸，后续 showTip 直接可用。
func (a *AppWindow) createTipWindow() {
	err := (MainWindow{
		AssignTo: &a.tipWin,
		Title:    "",
		// 抑制 Create() 末尾的自动 Show()，改由下面的 cloak 流程接管
		Visible: false,
		MinSize:  Size{Width: 400, Height: 54},
		MaxSize:  Size{Width: 400, Height: 54},
		Layout:   VBox{MarginsZero: true, SpacingZero: true},
		Children: []Widget{
			CustomWidget{
				MinSize: Size{Width: 400, Height: 54},
				PaintPixels: func(canvas *walk.Canvas, bounds walk.Rectangle) error {
					bg, _ := walk.NewSolidColorBrush(walk.RGB(255, 252, 220))
					defer bg.Dispose()
					_ = canvas.FillRectanglePixels(bg, bounds)
					pen, _ := walk.NewCosmeticPen(walk.PenSolid, walk.RGB(200, 0, 0))
					defer pen.Dispose()
					border := walk.Rectangle{X: 0, Y: 0, Width: bounds.Width - 1, Height: bounds.Height - 1}
					_ = canvas.DrawRectanglePixels(pen, border)
					font, _ := walk.NewFont("Segoe UI", 13, walk.FontBold)
					defer font.Dispose()
					_ = canvas.DrawTextPixels("外部链接工具，请自行甄别使用！！！", font,
						walk.RGB(200, 0, 0), bounds,
						walk.TextCenter|walk.TextVCenter|walk.TextSingleLine)
					return nil
				},
			},
		},
	}).Create()
	if err != nil || a.tipWin == nil {
		return
	}

	hwnd := a.tipWin.Handle()
	// 去掉标题栏、边框，改为纯弹出层。
	// 必须在下面那次布局用的 Show 之前做，否则会先闪一下带标题栏的样子。
	win.SetWindowLong(hwnd, win.GWL_STYLE, int32(win.WS_BORDER))
	win.SetWindowLong(hwnd, win.GWL_EXSTYLE,
		int32(win.WS_EX_TOPMOST|win.WS_EX_TOOLWINDOW))

	// 预热布局：cloak 住走一次真实的显示 → 布局 → 隐藏。
	// SWP_NOACTIVATE 保证不抢主窗口焦点；移到屏幕外再加一层保险。
	if !setWindowCloak(uintptr(hwnd), true) {
		// 不支持 cloak 的系统上不做预热，改为 showTip 时惰性布局（见 showTip）
		return
	}
	// 先挪到屏幕外并定好尺寸，再用 walk 的 Show()（而不是裸 SWP_SHOWWINDOW），
	// 这样 walk 内部的 visible 标记与真实状态一致，后续 Hide() 行为才正确。
	win.SetWindowPos(hwnd, win.HWND_TOPMOST, -32000, -32000, 400, 54,
		win.SWP_NOACTIVATE|win.SWP_NOZORDER)
	a.tipWin.Show()
	a.tipWin.RequestLayout()

	// 嵌套两层 Synchronize：walk 的布局结果由 RunSynchronized 在消息派发后回填，
	// 两轮之后布局一定已落地，此时再藏起来并解除 cloak。
	a.tipWin.Synchronize(func() {
		a.tipWin.Synchronize(func() {
			// 用 walk 的 Hide() 而不是裸 SetWindowPos，保持 walk 内部 visible 标记一致
			a.tipWin.Hide()
			setWindowCloak(uintptr(hwnd), false)
			a.tipPrewarmed = true
		})
	})
}

// showTip 在鼠标上方显示提示窗口，并启动轮询 goroutine 监控鼠标位置
func (a *AppWindow) showTip() {
	if a.tipWin == nil {
		return
	}
	// CAS: 0→1，防止重复启动
	if !atomic.CompareAndSwapInt32(&a.tipShown, 0, 1) {
		return
	}

	// 没能在创建时预热布局的情况（系统不支持 DWM cloak）：这里惰性补一次。
	// RequestLayout 是幂等的，多调一次没有副作用。
	if !a.tipPrewarmed {
		a.tipWin.RequestLayout()
	}

	var pt win.POINT
	win.GetCursorPos(&pt)
	const w, h int32 = 400, 54
	x := pt.X - w/2
	y := pt.Y - h - 10
	if x < 0 {
		x = 0
	}
	if y < 0 {
		y = pt.Y + 16
	}
	win.SetWindowPos(a.tipWin.Handle(), win.HWND_TOPMOST,
		x, y, w, h,
		win.SWP_NOACTIVATE|win.SWP_SHOWWINDOW)

	// 轮询 goroutine：每 20ms 检查鼠标是否还在标签区域内，离开立即隐藏
	go func() {
		for atomic.LoadInt32(&a.tipShown) == 1 {
			time.Sleep(20 * time.Millisecond)
			if !a.cursorInTag() {
				a.syncUI(func() { a.hideTip() })
				return
			}
		}
	}()
}

// hideTip 立即隐藏提示窗口
func (a *AppWindow) hideTip() {
	// CAS: 1→0，防止重复隐藏
	if !atomic.CompareAndSwapInt32(&a.tipShown, 1, 0) {
		return
	}
	if a.tipWin != nil {
		a.tipWin.Hide()
	}
}

// cursorInTag 判断鼠标当前屏幕坐标是否在右下角工具标签控件内
func (a *AppWindow) cursorInTag() bool {
	if a.toolWidget == nil {
		return false
	}
	var r win.RECT
	win.GetWindowRect(a.toolWidget.Handle(), &r)

	var pt win.POINT
	win.GetCursorPos(&pt)
	return pt.X >= r.Left && pt.X <= r.Right &&
		pt.Y >= r.Top && pt.Y <= r.Bottom
}
