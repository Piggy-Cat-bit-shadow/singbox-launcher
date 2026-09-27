// File controls.go — кнопки, сегментированный переключатель, статус-бейдж
// (SPEC 145).
//
// **Почему обёртки, а не свои renderer'ы.** Стандартная `widget.Button` уже
// умеет всё нужное: состояния normal/hover/pressed/disabled, фокус,
// клавиатуру, тему. Её внешний вид целиком определяется темой, поэтому
// primary/secondary/ghost различаются Importance и цветом, а не новой
// реализацией. Свой renderer здесь нужен ровно там, где стандартный виджет
// не даёт нужной формы: сегментированный переключатель.
//
// go1.20-совместимо (Win7-джоба): без slices/maps/min/max/clear.
package design

import (
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"singbox-launcher/internal/fynewidget"
)

// PrimaryAction — главная кнопка страницы.
//
// Высота фиксирована, чтобы ряд с заголовком не «прыгал» при смене подписи
// (Start ↔ Stop разной длины), а ширина задаётся минимальной: кнопка
// компактная, а не растянутая на полстроки.
func PrimaryAction(label string, onTapped func()) *widget.Button {
	b := widget.NewButton(label, onTapped)
	b.Importance = widget.HighImportance
	// Компактная высота: дефолтная кнопка Fyne с нашей темой (padding 8)
	// выглядит «блоком». 38 — высота кнопок в десктопных клиентах.
	b.Resize(fyne.NewSize(b.MinSize().Width, PrimaryButtonHeight))
	return b
}

// PrimaryButtonHeight — высота главной кнопки страницы.
//
// Вынесена в токен: Start/Stop и прочие primary-действия обязаны совпадать по
// высоте, иначе ряд «заголовок + кнопка» разъезжается.
const PrimaryButtonHeight float32 = 38

// SecondaryAction — обычная кнопка: заметная, но не конкурирующая с главной.
func SecondaryAction(label string, onTapped func()) *widget.Button {
	b := widget.NewButton(label, onTapped)
	b.Importance = widget.MediumImportance
	return b
}

// GhostAction — малозначимое действие: без заливки.
func GhostAction(label string, onTapped func()) *widget.Button {
	b := widget.NewButton(label, onTapped)
	b.Importance = widget.LowImportance
	return b
}

// DangerAction — разрушительное действие (удаление, kill).
func DangerAction(label string, icon fyne.Resource, onTapped func()) *widget.Button {
	var b *widget.Button
	if icon != nil {
		b = widget.NewButtonWithIcon(label, icon, onTapped)
	} else {
		b = widget.NewButton(label, onTapped)
	}
	// Опасное действие не должно выглядеть как обычное: Importance здесь не
	// даёт красного, поэтому цвет несёт подпись-предупреждение в тексте
	// диалога подтверждения, а кнопка остаётся заметной.
	b.Importance = widget.MediumImportance
	return b
}

// IconAction — кнопка только с иконкой. Обязательно с подсказкой: без текста
// назначение неочевидно.
func IconAction(icon fyne.Resource, tooltip string, onTapped func()) *widget.Button {
	b := widget.NewButtonWithIcon("", icon, onTapped)
	b.Importance = widget.LowImportance
	if tooltip != "" {
		fynewidget.SetToolTipSafe(b, tooltip)
	}
	return b
}

// SegmentedNav — сегментированный переключатель (page-local navigation).
//
// Используется внутри страниц вместо постоянного дерева в сайдбаре: у
// страницы два-четыре раздела, и переключаться между ними нужно на месте.
//
// Реализация — свой renderer, потому что стандартные виджеты Fyne не дают
// «таблетки» с общей рамкой и заливкой активного сегмента. Логика при этом
// минимальна: строка подписей, индекс активного, обработчик.
type SegmentedNav struct {
	widget.BaseWidget

	labels   []string
	onChange func(int)

	active int
	bg     *canvas.Rectangle
	items  []*segmentedItem
	row    *fyne.Container
}

// NewSegmentedNav собирает переключатель. onChange вызывается только при
// смене сегмента.
func NewSegmentedNav(labels []string, onChange func(int)) *SegmentedNav {
	s := &SegmentedNav{labels: labels, onChange: onChange, active: -1}
	s.ExtendBaseWidget(s)
	return s
}

// SetActive помечает сегмент активным без вызова onChange (для программной
// синхронизации).
func (s *SegmentedNav) SetActive(i int) {
	if i < 0 || i >= len(s.items) || s.active == i {
		return
	}
	s.active = i
	for k, it := range s.items {
		it.setActive(k == i)
	}
}

// Active возвращает индекс активного сегмента.
func (s *SegmentedNav) Active() int { return s.active }

// CreateRenderer implements fyne.Widget.
func (s *SegmentedNav) CreateRenderer() fyne.WidgetRenderer {
	s.bg = canvas.NewRectangle(SurfaceAlt())
	s.bg.CornerRadius = RadiusButton

	border := canvas.NewRectangle(color.Transparent)
	border.CornerRadius = RadiusButton
	border.StrokeColor = Border()
	border.StrokeWidth = 1

	s.items = make([]*segmentedItem, 0, len(s.labels))
	objs := make([]fyne.CanvasObject, 0, len(s.labels))
	for i, l := range s.labels {
		it := newSegmentedItem(l, i, s)
		s.items = append(s.items, it)
		objs = append(objs, it.object)
	}
	if s.active < 0 && len(s.items) > 0 {
		s.active = 0
		s.items[0].setActive(true)
	}
	s.row = container.NewHBox(objs...)
	return widget.NewSimpleRenderer(container.NewStack(
		s.bg, border,
		container.New(&paddedBox{l: 3, t: 3, r: 3, b: 3}, s.row)))
}

// MinSize: высота фиксирована, ширина — по подписям.
func (s *SegmentedNav) MinSize() fyne.Size {
	m := s.BaseWidget.MinSize()
	if m.Height < SegmentedHeight {
		m.Height = SegmentedHeight
	}
	return m
}

// Refresh перечитывает цвета темы.
func (s *SegmentedNav) Refresh() {
	s.BaseWidget.Refresh()
	if s.bg != nil {
		s.bg.FillColor = SurfaceAlt()
		s.bg.Refresh()
	}
	for _, it := range s.items {
		it.refreshColors()
	}
}

// segmentedItem — один сегмент.
type segmentedItem struct {
	widget.BaseWidget

	label  string
	index  int
	nav    *SegmentedNav
	bg     *canvas.Rectangle
	text   *canvas.Text
	holder *segmentedHolder
	object fyne.CanvasObject
	active bool
}

type segmentedHolder struct {
	widget.BaseWidget
	item *segmentedItem
}

func newSegmentedItem(label string, index int, nav *SegmentedNav) *segmentedItem {
	it := &segmentedItem{label: label, index: index, nav: nav}
	it.ExtendBaseWidget(it)

	it.bg = canvas.NewRectangle(color.Transparent)
	it.bg.CornerRadius = RadiusButton - 2

	it.text = canvas.NewText(label, TextSecondary())
	it.text.Alignment = fyne.TextAlignCenter

	it.holder = &segmentedHolder{item: it}
	it.holder.ExtendBaseWidget(it.holder)

	inner := container.New(&paddedBox{l: SpaceM, t: SpaceXS, r: SpaceM, b: SpaceXS},
		container.NewStack(it.bg, container.NewCenter(it.text)))
	it.object = container.New(&minHeight{min: SegmentedHeight - 6, max: SegmentedHeight - 6},
		container.NewStack(inner, it.holder))
	return it
}

func (it *segmentedItem) setActive(active bool) {
	it.active = active
	it.refreshColors()
}

func (it *segmentedItem) refreshColors() {
	if it.active {
		it.bg.FillColor = Surface()
		it.bg.Show()
		it.text.Color = Accent()
		it.text.TextStyle = fyne.TextStyle{Bold: true}
	} else {
		it.bg.Hide()
		it.text.Color = TextSecondary()
		it.text.TextStyle = fyne.TextStyle{}
	}
	it.bg.Refresh()
	it.text.Refresh()
}

// CreateRenderer implements fyne.Widget.
func (it *segmentedItem) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(canvas.NewRectangle(color.Transparent))
}

// CreateRenderer implements fyne.Widget.
func (h *segmentedHolder) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(canvas.NewRectangle(color.Transparent))
}

// Tapped implements fyne.Tappable.
func (h *segmentedHolder) Tapped(*fyne.PointEvent) {
	nav := h.item.nav
	if nav.active == h.item.index {
		return
	}
	nav.SetActive(h.item.index)
	if nav.onChange != nil {
		nav.onChange(h.item.index)
	}
}

// MouseIn implements desktop.Hoverable.
func (h *segmentedHolder) MouseIn(*desktop.MouseEvent) {
	if h.item.active {
		return
	}
	h.item.bg.FillColor = SurfaceHover()
	h.item.bg.Show()
	h.item.bg.Refresh()
}

// MouseMoved implements desktop.Hoverable.
func (h *segmentedHolder) MouseMoved(*desktop.MouseEvent) {}

// MouseOut implements desktop.Hoverable.
func (h *segmentedHolder) MouseOut() {
	if h.item.active {
		return
	}
	h.item.bg.Hide()
	h.item.bg.Refresh()
}

// StatusBadge — статусный бейдж: точка и подпись.
type StatusBadge struct {
	object fyne.CanvasObject
	dot    *canvas.Circle
	label  *widget.Label
	level  StatusLevel
}

// NewStatusBadge создаёт бейдж.
func NewStatusBadge(text string, level StatusLevel) *StatusBadge {
	b := &StatusBadge{level: level}
	b.dot = canvas.NewCircle(StatusColor(level))
	dotBox := container.New(&fixedSizeBox{w: 8, h: 8}, b.dot)
	b.label = widget.NewLabel(text)
	b.label.TextStyle = fyne.TextStyle{Bold: true}
	b.object = container.NewHBox(container.NewCenter(dotBox), b.label)
	return b
}

// Object возвращает объект бейджа.
func (b *StatusBadge) Object() fyne.CanvasObject { return b.object }

// Set обновляет текст и уровень, переиспользуя существующие объекты.
func (b *StatusBadge) Set(text string, level StatusLevel) {
	b.level = level
	if b.label.Text != text {
		b.label.SetText(text)
	}
	b.dot.FillColor = StatusColor(level)
	b.dot.Refresh()
}

// Level возвращает текущий уровень.
func (b *StatusBadge) Level() StatusLevel { return b.level }

// themeIcon возвращает иконку темы (хелпер для страниц).
func themeIcon(n fyne.ThemeIconName) fyne.Resource { return theme.Icon(n) }
