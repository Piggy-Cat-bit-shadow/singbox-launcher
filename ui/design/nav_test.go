package design

import (
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/test"
)

// parents — карта «объект → родитель», нужна для абсолютных координат:
// Fyne хранит Position() относительно родителя, а TapCanvas работает в
// абсолютных.
var parents = map[fyne.CanvasObject]fyne.CanvasObject{}

func indexParents(o fyne.CanvasObject, parent fyne.CanvasObject) {
	if o == nil {
		return
	}
	parents[o] = parent
	if c, ok := o.(*fyne.Container); ok {
		for _, ch := range c.Objects {
			indexParents(ch, o)
		}
	}
	if wd, ok := o.(fyne.Widget); ok {
		if r := test.WidgetRenderer(wd); r != nil {
			for _, ch := range r.Objects() {
				indexParents(ch, o)
			}
		}
	}
}

// absolutePos суммирует позиции всех родителей до корня канваса.
func absolutePos(o fyne.CanvasObject) fyne.Position {
	x, y := o.Position().X, o.Position().Y
	for p := parents[o]; p != nil; p = parents[p] {
		x += p.Position().X
		y += p.Position().Y
	}
	return fyne.NewPos(x, y)
}

// TestSidebarEveryItemIsClickable — регрессионный тест на «пункты сайдбара не
// нажимаются». Щёлкаем по геометрическому центру каждого пункта и проверяем,
// что выбран именно он.
func TestSidebarEveryItemIsClickable(t *testing.T) {
	app := test.NewApp()
	defer app.Quit()

	var got []NavID
	s := NewSidebar([]NavEntry{
		{ID: "home", Title: "Home", Section: "Overview"},
		{ID: "proxies", Title: "Proxies", Section: "Network"},
		{ID: "remote", Title: "Remote"},
		{ID: "traffic", Title: "Traffic"},
		{ID: "diag", Title: "Diagnostics", Section: "Tools"},
		{ID: "settings", Title: "Settings"},
		{ID: "about", Title: "About", Pinned: true},
	}, func(id NavID) { got = append(got, id) })

	root := container.NewStack(s)
	w := test.NewWindow(root)
	w.Resize(fyne.NewSize(SidebarWidth, 620))
	s.Refresh()
	parents = map[fyne.CanvasObject]fyne.CanvasObject{}
	indexParents(w.Canvas().Content(), nil)

	seen := map[NavID]bool{}
	for _, e := range s.entries {
		row := s.rows[e.ID]
		if row == nil {
			t.Fatalf("no row for %q", e.ID)
		}
		pos := absolutePos(row.object)
		size := row.object.Size()
		if size.Width <= 0 || size.Height <= 0 {
			t.Fatalf("%s has zero size", e.ID)
		}
		if !row.object.Visible() {
			t.Fatalf("%s is not visible", e.ID)
		}
		got = nil
		test.TapCanvas(w.Canvas(), fyne.NewPos(pos.X+size.Width/2, pos.Y+size.Height/2))
		if len(got) == 0 {
			t.Errorf("%-10s centre click did not reach the row", e.ID)
			continue
		}
		if got[0] != e.ID {
			t.Errorf("%-10s click selected %q", e.ID, got[0])
			continue
		}
		seen[e.ID] = true
	}
	if len(seen) != len(s.entries) {
		t.Fatalf("only %d of %d items were clickable", len(seen), len(s.entries))
	}
}

// TestSectionLabelsAreNotTappable — заголовки групп не должны быть
// кликабельны.
//
// Прямая защита от жалобы «Network выглядит как пункт, но не нажимается»:
// подпись группы собрана из canvas.Text, а не из widget.Button/Label внутри
// кликабельного держателя, поэтому клик по ней не делает ничего — и не должен.
//
// Проверяем структуру, а не поведение: считаем, что для каждой группы в
// колонке есть объект-подпись, не являющийся navRowHolder.
func TestSectionLabelsAreNotTappable(t *testing.T) {
	app := test.NewApp()
	defer app.Quit()

	s := NewSidebar([]NavEntry{
		{ID: "home", Title: "Home", Section: "Overview"},
		{ID: "proxies", Title: "Proxies", Section: "Network"},
		{ID: "remote", Title: "Remote"},
	}, nil)
	w := test.NewWindow(container.NewStack(s))
	w.Resize(fyne.NewSize(SidebarWidth, 400))
	s.Refresh()

	// Пунктов в колонке — три держателя (по одному на пункт), плюс подписи
	// групп. Если бы подпись была сделана пунктом, держателей было бы больше.
	holders := countHolders(s.body)
	if holders != 3 {
		t.Fatalf("found %d nav row holders for 3 items; section labels may have become clickable items", holders)
	}
	t.Logf("3 rows + section labels; no extra holder, so labels are not items")
}

// countHolders считает кликабельные держатели строк навигации.
func countHolders(o fyne.CanvasObject) int {
	if o == nil {
		return 0
	}
	n := 0
	if _, ok := o.(*navRowHolder); ok {
		n++
	}
	if c, ok := o.(*fyne.Container); ok {
		for _, ch := range c.Objects {
			n += countHolders(ch)
		}
	}
	if wd, ok := o.(fyne.Widget); ok {
		if r := test.WidgetRenderer(wd); r != nil {
			for _, ch := range r.Objects() {
				n += countHolders(ch)
			}
		}
	}
	return n
}
