// File layout.go — раскладки ограничения ширины и отступов (SPEC 145).
//
// **Зачем ограничивать ширину.** Если карточка растягивается на всю ширину
// окна, при 1400+ точках она выглядит сломанной: строка текста становится
// нечитаемо длинной, а элементы управления разъезжаются по краям. Современные
// клиенты держат контент в колонке фиксированной максимальной ширины и
// центрируют её на широком окне.
//
// **Ключевое требование к MinSize.** Ограничение ширины НЕ должно попадать в
// MinSize: иначе окно нельзя будет сжать меньше максимума. Поэтому
// MaxWidthLayout ограничивает только фактическую раскладку, а минимум
// делегирует содержимому.
//
// go1.20-совместимо (Win7-джоба): без slices/maps/min/max/clear.
package design

import (
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
)

// MaxWidthLayout ограничивает ширину содержимого и центрирует его.
type MaxWidthLayout struct {
	// Max — предельная ширина контентной колонки.
	Max float32
}

// Layout размещает содержимое по центру доступной ширины, не превышая Max.
func (m *MaxWidthLayout) Layout(objects []fyne.CanvasObject, size fyne.Size) {
	w := size.Width
	x := float32(0)
	if m.Max > 0 && w > m.Max {
		w = m.Max
		// Центрирование, а не выравнивание влево: на широком окне колонка
		// по центру читается как осознанный макет, а не как обрезанный.
		x = (size.Width - w) / 2
	}
	for _, o := range objects {
		o.Move(fyne.NewPos(x, 0))
		o.Resize(fyne.NewSize(w, size.Height))
	}
}

// MinSize возвращает минимум содержимого БЕЗ ограничения сверху: окно должно
// сжиматься свободно.
func (m *MaxWidthLayout) MinSize(objects []fyne.CanvasObject) fyne.Size {
	var min fyne.Size
	for _, o := range objects {
		s := o.MinSize()
		if s.Width > min.Width {
			min.Width = s.Width
		}
		if s.Height > min.Height {
			min.Height = s.Height
		}
	}
	return min
}

// ConstrainContent оборачивает содержимое в колонку максимальной ширины с
// полями страницы.
func ConstrainContent(content fyne.CanvasObject, maxWidth float32) fyne.CanvasObject {
	padded := container.New(&paddedBox{
		L: ContentPaddingH, T: ContentPaddingV,
		R: ContentPaddingH, B: ContentPaddingV,
	}, content)
	return container.New(&MaxWidthLayout{Max: maxWidth}, padded)
}

// hSpacer — горизонтальный распор фиксированной ширины.
func hSpacer(w float32) fyne.CanvasObject {
	r := canvas.NewRectangle(color.Transparent)
	r.SetMinSize(fyne.NewSize(w, 1))
	return r
}

// SpacerV — вертикальный распор заданной высоты.
func SpacerV(h float32) fyne.CanvasObject {
	r := canvas.NewRectangle(color.Transparent)
	r.SetMinSize(fyne.NewSize(1, h))
	return r
}

// MaxContentWidthWide — предел ширины для страниц со списками (Remote,
// Proxies). Шире обычных текстовых страниц: список узлов выигрывает от
// дополнительного места, тогда как форма настроек — нет.
const MaxContentWidthWide float32 = 900

// ConstrainContentWide — как ConstrainContent, но с широким пределом.
func ConstrainContentWide(content fyne.CanvasObject) fyne.CanvasObject {
	return ConstrainContent(content, MaxContentWidthWide)
}

// VStack — вертикальный список с ТОЧНЫМ промежутком.
//
// Отличие от container.NewVBox: последний берёт промежуток из
// theme.SizeNamePadding, поэтому объявленный в токенах gap не действует, а
// явно вставленный распор даёт двойной зазор. Здесь промежуток задаётся
// ровно один раз и не зависит от темы.
type VStack struct {
	Gap     float32
	Objects []fyne.CanvasObject
}

// NewVStack собирает вертикальный стек.
func NewVStack(gap float32, objects ...fyne.CanvasObject) *fyne.Container {
	return container.New(&VStack{Gap: gap, Objects: objects}, objects...)
}

// Layout размещает объекты сверху вниз с промежутком Gap.
func (v *VStack) Layout(objects []fyne.CanvasObject, size fyne.Size) {
	y := float32(0)
	for _, o := range objects {
		h := o.MinSize().Height
		o.Move(fyne.NewPos(0, y))
		o.Resize(fyne.NewSize(size.Width, h))
		y += h + v.Gap
	}
}

// MinSize складывает высоты и промежутки между ними.
func (v *VStack) MinSize(objects []fyne.CanvasObject) fyne.Size {
	var w, h float32
	for i, o := range objects {
		m := o.MinSize()
		if m.Width > w {
			w = m.Width
		}
		h += m.Height
		if i > 0 {
			h += v.Gap
		}
	}
	return fyne.NewSize(w, h)
}

// HStack — горизонтальный список с точным промежутком.
type HStack struct {
	Gap     float32
	Objects []fyne.CanvasObject
}

// NewHStack собирает горизонтальный стек.
func NewHStack(gap float32, objects ...fyne.CanvasObject) *fyne.Container {
	return container.New(&HStack{Gap: gap, Objects: objects}, objects...)
}

// Layout размещает объекты слева направо с промежутком Gap.
func (h *HStack) Layout(objects []fyne.CanvasObject, size fyne.Size) {
	x := float32(0)
	for _, o := range objects {
		w := o.MinSize().Width
		o.Move(fyne.NewPos(x, 0))
		o.Resize(fyne.NewSize(w, size.Height))
		x += w + h.Gap
	}
}

// MinSize складывает ширины и промежутки между ними.
func (h *HStack) MinSize(objects []fyne.CanvasObject) fyne.Size {
	var w, ht float32
	for i, o := range objects {
		m := o.MinSize()
		w += m.Width
		if i > 0 {
			w += h.Gap
		}
		if m.Height > ht {
			ht = m.Height
		}
	}
	return fyne.NewSize(w, ht)
}
