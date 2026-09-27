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
		l: ContentPaddingH, t: ContentPaddingV,
		r: ContentPaddingH, b: ContentPaddingV,
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
