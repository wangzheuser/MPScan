package gui

import (
	"path/filepath"

	"github.com/lxn/walk"
	"github.com/wux1an/wxapkg/core"
)

// ResultTableModel Walk TableView 数据模型，实现 CellStyler 接口进行颜色渲染
type ResultTableModel struct {
	walk.TableModelBase
	items []*core.ScanResult

	// tv 用来取当前 DPI。CellStyle 的 dpi 字段没有导出访问器，
	// 而图标必须按 TableView 的真实 DPI 构造，否则取不到句柄、整列不显示。
	tv *walk.TableView
}

func newResultTableModel() *ResultTableModel {
	return &ResultTableModel{items: make([]*core.ScanResult, 0, 64)}
}

// dpi 返回 TableView 当前 DPI；还没绑定时退回 96。
func (m *ResultTableModel) dpi() int {
	if m.tv != nil {
		if d := m.tv.DPI(); d > 0 {
			return d
		}
	}
	return 96
}

func (m *ResultTableModel) RowCount() int { return len(m.items) }

func (m *ResultTableModel) Value(row, col int) interface{} {
	if row < 0 || row >= len(m.items) {
		return nil
	}
	r := m.items[row]
	switch col {
	case 0:
		return r.AppName
	case 1:
		return string(r.Level)
	case 2:
		return r.Category
	case 3:
		return r.KeyName
	case 4:
		return r.Value
	case 5:
		return filepath.Base(r.FilePath)
	}
	return nil
}

// StyleCell 按风险等级着色（实现 walk.CellStyler 接口）
func (m *ResultTableModel) StyleCell(style *walk.CellStyle) {
	if style.Row() < 0 || style.Row() >= len(m.items) {
		return
	}

	// 第一列显示小程序官方图标（来自微信缓存的 ico 目录，路径由监控目录相对推出）。
	// 名称没解析出来、列里只剩 wxid 时，靠图标确认资产归属。
	//
	// 没有对应图标的行会拿到透明占位图标而不是 nil，这样每行都有确定的图标索引
	// （walk 只在 image != nil 时才写 di.Item.IImage），而且各行文字左边距一致，
	// 不会因为有的行有图标、有的行没有而参差不齐。
	if style.Col() == 0 {
		// DPI 必须和 walk 取句柄时用的一致（walk 用 tv.DPI()），
		// 构造时用的 DPI 不一致会取到 0，整列图标都不会显示。
		if ic := iconForWxID(m.items[style.Row()].WxID, m.dpi()); ic != nil {
			style.Image = ic
		}
	}

	switch m.items[style.Row()].Level {
	case core.LevelHigh:
		style.BackgroundColor = walk.RGB(255, 230, 230) // 浅红
		style.TextColor = walk.RGB(180, 0, 0)
	case core.LevelMedium:
		style.BackgroundColor = walk.RGB(255, 248, 220) // 浅橙
		style.TextColor = walk.RGB(150, 90, 0)
	case core.LevelLow:
		style.BackgroundColor = walk.RGB(230, 243, 255) // 浅蓝
		style.TextColor = walk.RGB(0, 80, 160)
	}
}

func (m *ResultTableModel) appendItems(items []*core.ScanResult) {
	if len(items) == 0 {
		return
	}
	m.items = append(m.items, items...)
	m.PublishRowsReset()
}

func (m *ResultTableModel) clear() {
	m.items = m.items[:0]
	m.PublishRowsReset()
}

func (m *ResultTableModel) getItem(row int) *core.ScanResult {
	if row < 0 || row >= len(m.items) {
		return nil
	}
	return m.items[row]
}

func (m *ResultTableModel) stats() (high, mid, low int) {
	for _, r := range m.items {
		switch r.Level {
		case core.LevelHigh:
			high++
		case core.LevelMedium:
			mid++
		case core.LevelLow:
			low++
		}
	}
	return
}
