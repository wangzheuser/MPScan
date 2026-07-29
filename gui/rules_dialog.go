package gui

import (
	"fmt"
	"os/exec"
	"strconv"
	"strings"

	"unsafe"

	"github.com/lxn/walk"
	dcl "github.com/lxn/walk/declarative"
	"github.com/lxn/win"
	"github.com/wux1an/wxapkg/core"
)

// ruleTableModel 规则列表的表格模型
type ruleTableModel struct {
	walk.TableModelBase
	items []core.Rule
}

func (m *ruleTableModel) RowCount() int { return len(m.items) }

func (m *ruleTableModel) Value(row, col int) interface{} {
	if row < 0 || row >= len(m.items) {
		return ""
	}
	r := m.items[row]
	switch col {
	case 0:
		return r.Enabled
	case 1:
		if r.Builtin {
			return "内置"
		}
		return "自定义"
	case 2:
		return r.Category
	case 3:
		return r.KeyName
	case 4:
		return r.Level
	case 5:
		return r.ValueGroup
	case 6:
		return r.Pattern
	}
	return ""
}

// Checked / SetChecked 让第一列成为可直接勾选的启用开关
func (m *ruleTableModel) Checked(row int) bool {
	if row < 0 || row >= len(m.items) {
		return false
	}
	return m.items[row].Enabled
}

func (m *ruleTableModel) SetChecked(row int, checked bool) error {
	if row < 0 || row >= len(m.items) {
		return nil
	}
	m.items[row].Enabled = checked
	return nil
}

func (m *ruleTableModel) StyleCell(style *walk.CellStyle) {
	if style.Row() < 0 || style.Row() >= len(m.items) {
		return
	}
	r := m.items[style.Row()]
	if !r.Enabled {
		style.TextColor = walk.RGB(150, 150, 150)
		return
	}
	if style.Col() == 4 {
		switch r.Level {
		case "高危":
			style.TextColor = walk.RGB(200, 30, 30)
		case "中危":
			style.TextColor = walk.RGB(200, 120, 0)
		default:
			style.TextColor = walk.RGB(80, 80, 80)
		}
	}
}

// ShowRulesDialog 打开「规则管理」窗口。
// 支持修改内置规则、新增自定义规则、启用/停用、恢复默认，保存后立即对后续扫描生效。
func ShowRulesDialog(owner walk.Form) {
	model := &ruleTableModel{items: core.CurrentRules()}

	var dlg *walk.Dialog
	var tv *walk.TableView
	var closeBtn *walk.PushButton
	dirty := false

	markDirty := func() { dirty = true }

	// 对话框尺寸必须按屏幕工作区自适应，否则高缩放下会开成一个"空窗口"。
	//
	// walk 的 Dialog.Show() 在有 Layout 时，尺寸取
	//     maxSize(clientComposite.MinSizeHint(), MinSizePixels())
	// —— 声明式的 Size 字段被完全忽略，真正决定窗口大小的是 MinSize。
	// 而 MinSize 是 96dpi 逻辑值，会乘 DPI/96：150% 缩放下写死 1000x560
	// 会变成 1500x840。此时 fitRectToScreen 又只在"装得下"时才挪位置
	// （if r.Height <= mon.Height，且 mon.Height 还要减掉标题栏高度），
	// 装不下就原样放置，于是底部整排按钮和右侧列被推到屏幕外，
	// 用户看到的就是中间一片空白表格。
	//
	// 所以这里把自适应结果写进 MinSize。
	minW, minH := fitDialogSize96dpi(1000, 560)

	err := dcl.Dialog{
		AssignTo: &dlg,
		Title:    "规则管理 - 敏感信息正则",
		// 标题栏图标跟主界面保持一致（同一个内嵌 app.ico）
		Icon:    AppIcon(),
		MinSize: dcl.Size{Width: minW, Height: minH},
		Layout:  dcl.VBox{MarginsZero: false},
		Children: []dcl.Widget{
			dcl.Label{
				Text: "勾选左侧复选框可启用/停用规则；双击任意行编辑。修改内置规则不会丢失，可随时「恢复默认」。",
			},
			dcl.TableView{
				AssignTo:            &tv,
				Model:               model,
				CheckBoxes:          true,
				AlternatingRowBG:    true,
				ColumnsOrderable:    true,
				MultiSelection:      false,
				LastColumnStretched: true,
				// 列宽同样是 96dpi 逻辑值。总和收窄到约 660，
				// 这样即使在 175% 缩放下也不会横向溢出；
				// 「正则表达式」列由 LastColumnStretched 吃掉剩余宽度。
				Columns: []dcl.TableViewColumn{
					{Title: "启用", Width: 44},
					{Title: "来源", Width: 56},
					{Title: "分类", Width: 130},
					{Title: "键名", Width: 110},
					{Title: "等级", Width: 48},
					{Title: "取值组", Width: 52},
					{Title: "正则表达式", Width: 220},
				},
				OnItemActivated: func() {
					i := tv.CurrentIndex()
					if i < 0 || i >= len(model.items) {
						return
					}
					r := model.items[i]
					if editRuleDialog(dlg, &r) == walk.DlgCmdOK {
						model.items[i] = r
						model.PublishRowChanged(i)
						markDirty()
					}
				},
				OnCurrentIndexChanged: func() {},
			},
			dcl.Composite{
				Layout: dcl.HBox{MarginsZero: true},
				Children: []dcl.Widget{
					dcl.PushButton{
						Text: "新增规则",
						OnClicked: func() {
							r := core.Rule{Level: "中危", ValueGroup: 1, Enabled: true}
							if editRuleDialog(dlg, &r) == walk.DlgCmdOK {
								r.Builtin = false
								if strings.TrimSpace(r.ID) == "" {
									r.ID = newRuleID(model.items)
								}
								model.items = append(model.items, r)
								model.PublishRowsReset()
								tv.SetCurrentIndex(len(model.items) - 1)
								markDirty()
							}
						},
					},
					dcl.PushButton{
						Text: "编辑",
						OnClicked: func() {
							i := tv.CurrentIndex()
							if i < 0 || i >= len(model.items) {
								walk.MsgBox(dlg, "提示", "请先选中一条规则", walk.MsgBoxIconInformation)
								return
							}
							r := model.items[i]
							if editRuleDialog(dlg, &r) == walk.DlgCmdOK {
								model.items[i] = r
								model.PublishRowChanged(i)
								markDirty()
							}
						},
					},
					dcl.PushButton{
						Text: "删除",
						OnClicked: func() {
							i := tv.CurrentIndex()
							if i < 0 || i >= len(model.items) {
								walk.MsgBox(dlg, "提示", "请先选中一条规则", walk.MsgBoxIconInformation)
								return
							}
							r := model.items[i]
							msg := fmt.Sprintf("确定删除规则「%s」？", r.Category)
							if r.Builtin {
								msg = fmt.Sprintf("「%s」是内置规则，删除后可通过「恢复默认」找回。\n确定删除？", r.Category)
							}
							if walk.MsgBox(dlg, "确认删除", msg,
								walk.MsgBoxYesNo|walk.MsgBoxIconQuestion) != walk.DlgCmdYes {
								return
							}
							model.items = append(model.items[:i], model.items[i+1:]...)
							model.PublishRowsReset()
							markDirty()
						},
					},
					dcl.PushButton{
						Text: "恢复默认",
						OnClicked: func() {
							if walk.MsgBox(dlg, "恢复默认",
								"将丢弃所有自定义规则和改动，恢复为内置规则集。\n确定继续？",
								walk.MsgBoxYesNo|walk.MsgBoxIconWarning) != walk.DlgCmdYes {
								return
							}
							rs, rerr := core.ResetRules()
							if rerr != nil {
								walk.MsgBox(dlg, "失败", rerr.Error(), walk.MsgBoxIconError)
								return
							}
							model.items = rs
							model.PublishRowsReset()
							dirty = false
						},
					},
					dcl.PushButton{
						Text: "打开规则文件",
						OnClicked: func() {
							p := core.RulesFilePath()
							if err := exec.Command("cmd", "/c", "start", "", p).Start(); err != nil {
								walk.MsgBox(dlg, "规则文件位置", p, walk.MsgBoxIconInformation)
							}
						},
					},
					dcl.HSpacer{},
					dcl.PushButton{
						Text: "保存并生效",
						OnClicked: func() {
							if err := core.SaveRules(model.items); err != nil {
								walk.MsgBox(dlg, "保存失败", err.Error(), walk.MsgBoxIconError)
								return
							}
							dirty = false
							walk.MsgBox(dlg, "已保存",
								fmt.Sprintf("共 %d 条规则已保存，下次扫描立即生效。", len(model.items)),
								walk.MsgBoxIconInformation)
						},
					},
					dcl.PushButton{
						AssignTo: &closeBtn,
						Text:     "关闭",
						OnClicked: func() {
							if dirty {
								if walk.MsgBox(dlg, "未保存的改动",
									"有改动尚未保存，直接关闭将丢弃。\n确定关闭？",
									walk.MsgBoxYesNo|walk.MsgBoxIconWarning) != walk.DlgCmdYes {
									return
								}
							}
							dlg.Accept()
						},
					},
				},
			},
		},
	}.Create(owner)

	if err != nil {
		walk.MsgBox(owner, "打开失败", err.Error(), walk.MsgBoxIconError)
		return
	}
	if closeBtn != nil {
		dlg.SetDefaultButton(closeBtn)
		dlg.SetCancelButton(closeBtn)
	}
	dlg.Run()
}

// fitDialogSize96dpi 把期望的 96dpi 逻辑尺寸压到当前屏幕装得下的范围内。
//
// 传入/返回的都是 96dpi 逻辑像素（walk 创建窗口时会乘 DPI/96）。
// 高缩放下写死的逻辑尺寸会被放大到超出屏幕，而带 Layout 的 Dialog
// 尺寸由 MinSize 决定、又缩不回来，所以必须按真实工作区反算上限。
//
// 竖向要比横向多留一些：walk 的 fitRectToScreen 判断"装不装得下"时，
// 会先把可用高度减掉一个标题栏高度（SM_CYCAPTION，144dpi 下约 33px），
// 边距不够就会退化成"原样放置"，底部按钮被推出屏幕。
// 取不到工作区或 DPI 时原样返回，退化成原来的行为。
func fitDialogSize96dpi(wantW, wantH int) (int, int) {
	// lxn/win 没导出 SPI_GETWORKAREA
	const spiGetWorkArea = 0x0030

	var wa win.RECT
	if !win.SystemParametersInfo(spiGetWorkArea, 0, unsafe.Pointer(&wa), 0) {
		return wantW, wantH
	}
	availW := int(wa.Right - wa.Left)
	availH := int(wa.Bottom - wa.Top)
	if availW <= 0 || availH <= 0 {
		return wantW, wantH
	}

	dpi := screenDPI()
	if dpi <= 0 {
		return wantW, wantH
	}

	// 物理像素边距：横向留出边框/阴影，纵向额外让出标题栏 + 余量
	const marginW = 48
	marginH := 48 + int(win.GetSystemMetrics(win.SM_CYCAPTION))
	maxW96 := (availW - marginW) * 96 / dpi
	maxH96 := (availH - marginH) * 96 / dpi

	if wantW > maxW96 {
		wantW = maxW96
	}
	if wantH > maxH96 {
		wantH = maxH96
	}
	// 再兜一层最小可用尺寸，防止极端小屏算出不可用的值
	if wantW < 480 {
		wantW = 480
	}
	if wantH < 320 {
		wantH = 320
	}
	return wantW, wantH
}

// screenDPI 返回主屏的有效 DPI（96 = 100% 缩放）。
func screenDPI() int {
	hdc := win.GetDC(0)
	if hdc == 0 {
		return 0
	}
	defer win.ReleaseDC(0, hdc)
	return int(win.GetDeviceCaps(hdc, win.LOGPIXELSX))
}

// newRuleID 为新增规则生成不重复的 ID
func newRuleID(existing []core.Rule) string {
	used := make(map[string]bool, len(existing))
	for _, r := range existing {
		used[r.ID] = true
	}
	for i := 1; ; i++ {
		id := "custom-" + strconv.Itoa(i)
		if !used[id] {
			return id
		}
	}
}

// editRuleDialog 单条规则的编辑窗口，返回 walk.DlgCmdOK 表示用户确认保存。
// 确认前会走 core.ValidateRule 校验，正则写错会当场提示（含 RE2 不支持环视的说明）。
func editRuleDialog(owner walk.Form, r *core.Rule) int {
	var dlg *walk.Dialog
	var okBtn, cancelBtn *walk.PushButton
	var catEdit, keyEdit, patEdit, groupEdit, sampleEdit *walk.LineEdit
	var levelCB *walk.ComboBox
	var enabledCB *walk.CheckBox

	levels := []string{"高危", "中危", "低危"}
	levelIdx := 1
	for i, l := range levels {
		if l == r.Level {
			levelIdx = i
		}
	}

	title := "编辑规则"
	if strings.TrimSpace(r.Category) == "" {
		title = "新增规则"
	}

	collect := func() core.Rule {
		out := *r
		out.Category = strings.TrimSpace(catEdit.Text())
		out.KeyName = strings.TrimSpace(keyEdit.Text())
		out.Pattern = patEdit.Text()
		out.Enabled = enabledCB.Checked()
		if i := levelCB.CurrentIndex(); i >= 0 && i < len(levels) {
			out.Level = levels[i]
		}
		g, gerr := strconv.Atoi(strings.TrimSpace(groupEdit.Text()))
		if gerr != nil {
			g = 0
		}
		out.ValueGroup = g
		return out
	}

	editW, editH := fitDialogSize96dpi(660, 340)

	err := dcl.Dialog{
		AssignTo:      &dlg,
		Title:         title,
		Icon:          AppIcon(),
		DefaultButton: &okBtn,
		CancelButton:  &cancelBtn,
		// 同样走自适应，避免高缩放下编辑窗口超出屏幕（见 fitDialogSize96dpi）
		MinSize: dcl.Size{Width: editW, Height: editH},
		Layout:  dcl.VBox{},
		Children: []dcl.Widget{
			dcl.Composite{
				Layout: dcl.Grid{Columns: 2},
				Children: []dcl.Widget{
					dcl.Label{Text: "分类："},
					dcl.LineEdit{AssignTo: &catEdit, Text: r.Category, CueBanner: "例如 微信·AppSecret"},

					dcl.Label{Text: "键名："},
					dcl.LineEdit{AssignTo: &keyEdit, Text: r.KeyName, CueBanner: "例如 AppSecret"},

					dcl.Label{Text: "风险等级："},
					dcl.ComboBox{AssignTo: &levelCB, Model: levels, CurrentIndex: levelIdx},

					dcl.Label{Text: "正则表达式："},
					dcl.LineEdit{AssignTo: &patEdit, Text: r.Pattern,
						CueBanner: `Go RE2 语法，如 (?i)"?key"?\s*[:=]\s*"([a-z0-9]{32})"`},

					dcl.Label{Text: "取值捕获组："},
					dcl.LineEdit{AssignTo: &groupEdit, Text: strconv.Itoa(r.ValueGroup),
						CueBanner: "0=整个匹配，1=第一个括号"},

					dcl.Label{Text: "启用："},
					dcl.CheckBox{AssignTo: &enabledCB, Checked: r.Enabled},

					dcl.Label{Text: "测试文本："},
					dcl.LineEdit{AssignTo: &sampleEdit, CueBanner: "粘贴一行样本，点「测试匹配」验证效果"},
				},
			},
			dcl.Label{
				Text:      "提示：Go 使用 RE2 引擎，不支持 (?=...) (?!...) 等环视语法。取值组决定结果里展示哪一段内容。",
				TextColor: walk.RGB(110, 110, 110),
			},
			dcl.VSpacer{},
			dcl.Composite{
				Layout: dcl.HBox{MarginsZero: true},
				Children: []dcl.Widget{
					dcl.PushButton{
						Text: "测试匹配",
						OnClicked: func() {
							cur := collect()
							sample := sampleEdit.Text()
							if strings.TrimSpace(sample) == "" {
								walk.MsgBox(dlg, "测试", "请先填写测试文本", walk.MsgBoxIconInformation)
								return
							}
							val, terr := core.TestRule(cur, sample)
							if terr != nil {
								walk.MsgBox(dlg, "测试结果", terr.Error(), walk.MsgBoxIconWarning)
								return
							}
							walk.MsgBox(dlg, "测试结果", "命中，取到的值：\n\n"+val, walk.MsgBoxIconInformation)
						},
					},
					dcl.HSpacer{},
					dcl.PushButton{
						AssignTo: &okBtn,
						Text:     "确定",
						OnClicked: func() {
							cur := collect()
							if verr := core.ValidateRule(cur); verr != nil {
								walk.MsgBox(dlg, "规则无效", verr.Error(), walk.MsgBoxIconError)
								return
							}
							*r = cur
							dlg.Accept()
						},
					},
					dcl.PushButton{
						AssignTo:  &cancelBtn,
						Text:      "取消",
						OnClicked: func() { dlg.Cancel() },
					},
				},
			},
		},
	}.Create(owner)
	if err != nil {
		walk.MsgBox(owner, "打开失败", err.Error(), walk.MsgBoxIconError)
		return walk.DlgCmdCancel
	}
	return dlg.Run()
}
