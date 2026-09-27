// File sidebar.go — плоская навигационная колонка (SPEC 145).
//
// **Что изменилось относительно SPEC 144.** Прошлая версия строила
// двухуровневое дерево (родитель + постоянные подпункты) и подсвечивала
// выбор accent-полосой 3 unit слева. Визуально это паттерн веб-админки, а не
// macOS-утилиты. Здесь:
//
//   - навигация ПЛОСКАЯ: один уровень пунктов, сгруппированных заголовками;
//   - выбранный пункт — мягкая скруглённая заливка (pill), без полосы;
//   - иконка и текст выбранного пункта окрашены акцентом;
//   - детализация уезжает в page-local navigation, а не висит в сайдбаре.
//
// **Три дефекта прошлой версии, устранённые здесь:**
//
//  1. Accent-полоса была полновысотным слоем `Stack` без ограничения ширины
//     и заливала строку целиком. Полосы больше нет.
//  2. Подпись обрезалась (`TextTruncateEllipsis`) при жёстком `MinSize`
//     колонки — при длинных строках и в русской локали пункт выглядел как
//     «…». Теперь перенос по словам, а высота строки допускает две строки.
//  3. `MinSize` колонки складывался из высоты содержимого и мог «прыгать».
//     Теперь высота строк фиксирована, и колонка не зависит от метрик шрифта
//     конкретной ОС.
//
// **Состояние.** Сайдбар знает только о навигации: какой пункт выбран. Он
// НЕ знает, запущено ли ядро или подключена ли машина — это проекция
// AppController.RunningState, передаваемая снаружи через SetStatus.
//
// go1.20-совместимо (Win7-джоба): без slices/maps/min/max/clear.
package design

import (
	"image/color"
	"strings"

	"fyne.io/fyne/v2/theme"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/widget"

	"singbox-launcher/internal/constants"
	"singbox-launcher/internal/fynewidget"
)

// NavID — идентификатор пункта навигации.
type NavID string

// NavEntry — один пункт навигации. Иерархии нет: пункт либо есть, либо нет.
type NavEntry struct {
	// ID — идентификатор маршрута.
	ID NavID
	// Title — подпись.
	Title string
	// Icon — иконка. Обязательна: плоская навигация без иконок читается хуже.
	Icon fyne.Resource
	// Section — заголовок группы, к которой относится пункт. Пусто —
	// продолжение предыдущей группы.
	Section string
	// Pinned — пункт прижат к низу колонки (About).
	Pinned bool
}

// Sidebar — навигационная колонка.
type Sidebar struct {
	widget.BaseWidget

	entries  []NavEntry
	onSelect func(NavID)

	selected NavID
	rows     map[NavID]*navRow

	body *fyne.Container

	dot         *canvas.Circle
	statusLabel *widget.Label
}

var _ fyne.Widget = (*Sidebar)(nil)

// NewSidebar собирает колонку.
//
// onSelect вызывается на UI-потоке при выборе пункта и только при СМЕНЕ
// выбора: повторный клик по активному пункту ничего не делает.
func NewSidebar(entries []NavEntry, onSelect func(NavID)) *Sidebar {
	s := &Sidebar{
		entries:  entries,
		onSelect: onSelect,
		rows:     make(map[NavID]*navRow),
	}
	s.ExtendBaseWidget(s)
	s.build()
	return s
}

// SetSelected помечает пункт активным. Бизнес-состояние не трогается.
func (s *Sidebar) SetSelected(id NavID) {
	s.selected = id
	for _, r := range s.rows {
		r.setActive(r.entry.ID == id)
	}
}

// Selected возвращает активный пункт.
func (s *Sidebar) Selected() NavID { return s.selected }

// build конструирует строки один раз. Дальнейшие изменения состояния лишь
// перекрашивают существующие объекты: при клике новые CanvasObject не
// создаются.
func (s *Sidebar) build() {
	var main, pinned []fyne.CanvasObject
	var lastSection string

	for _, e := range s.entries {
		row := newNavRow(e, s)
		s.rows[e.ID] = row
		if e.Pinned {
			pinned = append(pinned, row.object)
			continue
		}
		if e.Section != "" && e.Section != lastSection {
			main = append(main, navSectionLabel(e.Section))
			lastSection = e.Section
		}
		main = append(main, row.object)
	}

	body := container.NewVBox(main...)
	if len(pinned) > 0 {
		body.Add(widget.NewSeparator())
		body.Add(container.NewVBox(pinned...))
	}
	s.body = body
}

// selectItem — клик по пункту.
func (s *Sidebar) selectItem(id NavID) {
	if id == s.selected {
		return
	}
	s.SetSelected(id)
	if s.onSelect != nil {
		s.onSelect(id)
	}
}

// CreateRenderer implements fyne.Widget.
func (s *Sidebar) CreateRenderer() fyne.WidgetRenderer {
	// Фон колонки отличается от фона окна: это и отделяет навигацию от
	// контента, без рамок и разделителей.
	bg := canvas.NewRectangle(SidebarSurface())
	// Единственная линия на всю колонку — на её правой границе. Внутри
	// колонки линий нет.
	edge := canvas.NewRectangle(Border())
	edge.SetMinSize(fyne.NewSize(1, 0))

	identity := container.New(&fixedHeight{min: SidebarIdentityHeight},
		container.New(&paddedBox{l: SidebarPadding + SpaceS, t: 0, r: SidebarPadding, b: 0},
			container.NewCenter(container.NewHBox(sidebarAppIcon(), hSpacer(SpaceS), sidebarIdentityLabel()))))

	scroll := container.NewVScroll(container.NewVBox(
		identity,
		container.New(&paddedBox{l: SidebarPadding, t: 0, r: SidebarPadding, b: SidebarPadding}, s.body),
	))

	content := container.NewBorder(nil, s.footerRow(), nil, edge, scroll)
	return widget.NewSimpleRenderer(container.NewStack(bg, content))
}

// footerRow — нижняя строка колонки: статус ядра.
func (s *Sidebar) footerRow() fyne.CanvasObject {
	s.dot = canvas.NewCircle(StatusColor(StatusNeutral))
	dotBox := container.New(&fixedSizeBox{w: 8, h: 8}, s.dot)
	s.statusLabel = widget.NewLabel("—")
	s.statusLabel.Importance = widget.LowImportance
	// Отсутствие truncation здесь принципиально: раньше подпись статуса
	// обрезалась и «Disconnected» превращался в «Disc…».
	s.statusLabel.Wrapping = fyne.TextWrapWord
	return container.New(&paddedBox{l: SidebarPadding + SpaceS, t: SpaceM, r: SidebarPadding, b: SpaceM},
		container.NewHBox(container.NewCenter(dotBox), s.statusLabel))
}

// SetStatus обновляет статус ядра в нижней строке.
//
// Presentation-проекция: источник истины — AppController.RunningState.
func (s *Sidebar) SetStatus(text string, level StatusLevel) {
	if s.statusLabel == nil {
		return
	}
	if s.statusLabel.Text != text {
		s.statusLabel.SetText(text)
	}
	if s.dot != nil {
		s.dot.FillColor = StatusColor(level)
		s.dot.Refresh()
	}
}

// MinSize задаёт ширину колонки. Высота не ограничивается: содержимое
// прокручивается, и колонка не «прыгает» при смене локали.
func (s *Sidebar) MinSize() fyne.Size {
	return fyne.NewSize(SidebarWidth, SidebarIdentityHeight+4*NavItemHeight)
}

// --- Строка пункта ---------------------------------------------------------

type navRow struct {
	entry   NavEntry
	sidebar *Sidebar

	object fyne.CanvasObject
	bg     *canvas.Rectangle
	label  *widget.Label
	icon   *widget.Icon

	active bool
	holder *navRowHolder
}

// navRowHolder перехватывает клики и наведение.
type navRowHolder struct {
	widget.BaseWidget
	row    *navRow
	object fyne.CanvasObject
}

func newNavRow(e NavEntry, s *Sidebar) *navRow {
	r := &navRow{entry: e, sidebar: s}

	// Скруглённая заливка на всю строку. Никакой полосы слева: выбранное
	// состояние выражается заливкой, цветом текста и цветом иконки.
	r.bg = canvas.NewRectangle(nil)
	r.bg.CornerRadius = RadiusNav
	r.bg.Hide()

	r.icon = widget.NewIcon(tintIcon(e.Icon, TextSecondary()))

	r.label = widget.NewLabel(e.Title)
	r.label.Alignment = fyne.TextAlignLeading
	// Перенос, а не ellipsis: длинная подпись (русская локализация) должна
	// переноситься на вторую строку, а не превращаться в «…».
	r.label.Wrapping = fyne.TextWrapWord
	r.label.Importance = widget.LowImportance

	body := container.NewBorder(nil, nil, container.NewHBox(r.icon, r.label), nil)

	holder := &navRowHolder{row: r}
	holder.ExtendBaseWidget(holder)
	r.holder = holder

	inner := container.New(&paddedBox{l: SpaceM, t: SpaceXS, r: SpaceM, b: SpaceXS}, body)
	stack := container.NewStack(r.bg, inner)
	holder.object = container.NewStack(stack, holder)

	// Высота фиксирована снизу и ограничена сверху: строка не должна менять
	// размер при наведении или выборе (иначе раскладка «дышит»), но длинная
	// подпись в две строки обязана поместиться.
	r.object = container.New(&minHeight{min: NavItemHeight, max: NavItemHeight * 2}, holder.object)
	fynewidget.SetToolTipSafe(r.object, e.Title)
	return r
}

// setActive переключает подсветку выбранного пункта.
func (r *navRow) setActive(active bool) {
	if r.active == active {
		return
	}
	r.active = active
	if active {
		r.bg.FillColor = SurfaceSelected()
		r.bg.Show()
		r.label.TextStyle = fyne.TextStyle{Bold: true}
		r.label.Importance = widget.MediumImportance
		r.icon.Resource = tintIcon(r.entry.Icon, Accent())
	} else {
		r.bg.Hide()
		r.label.TextStyle = fyne.TextStyle{}
		r.label.Importance = widget.LowImportance
		r.icon.Resource = tintIcon(r.entry.Icon, TextSecondary())
	}
	r.label.Refresh()
	r.icon.Refresh()
	r.bg.Refresh()
	canvas.Refresh(r.holder)
}

// applyHover перекрашивает фон под курсором. Активный пункт сохраняет свою
// заливку: hover не должен «гасить» выбор.
func (r *navRow) applyHover(hovered bool) {
	switch {
	case r.active:
		r.bg.FillColor = SurfaceSelected()
		r.bg.Show()
	case hovered:
		r.bg.FillColor = SurfaceHover()
		r.bg.Show()
	default:
		r.bg.Hide()
	}
	r.bg.Refresh()
}

// CreateRenderer implements fyne.Widget.
func (h *navRowHolder) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(canvas.NewRectangle(nil))
}

// Tapped implements fyne.Tappable.
func (h *navRowHolder) Tapped(*fyne.PointEvent) { h.row.sidebar.selectItem(h.row.entry.ID) }

// MouseIn implements desktop.Hoverable.
func (h *navRowHolder) MouseIn(*desktop.MouseEvent) { h.row.applyHover(true) }

// MouseMoved implements desktop.Hoverable.
func (h *navRowHolder) MouseMoved(*desktop.MouseEvent) {}

// MouseOut implements desktop.Hoverable.
func (h *navRowHolder) MouseOut() { h.row.applyHover(false) }

// --- Мелкие части ----------------------------------------------------------

// navSectionLabel — заголовок группы. Мелкий, приглушённый, без начертания:
// он ориентирует, но не конкурирует с пунктами за внимание.
func navSectionLabel(text string) fyne.CanvasObject {
	// Заголовок группы обязан читаться как ПОДПИСЬ, а не как пункт меню.
	//
	// Признаки, отличающие его от nav item (все обязательны — одного мало,
	// иначе пользователь всё равно пробует кликнуть):
	//
	//   1. НЕТ плашки, hover и selected-состояния — в отличие от пунктов,
	//      которые подсвечиваются при наведении. Отсутствие реакции на
	//      курсор — главный сигнал.
	//   2. Мелкий текст: canvas.Text с уменьшенным кеглем, а не widget.Label
	//      того же размера, что и подписи пунктов.
	//   3. Другой отступ от края: подпись выровнена не по иконкам пунктов,
	//      поэтому не встаёт в их вертикальную сетку.
	//   4. Верхний отступ больше нижнего: заголовок «прилипает» к своей
	//      группе, а не висит между группами.
	//
	// Отдельный объект — canvas.Text, а не Label: только так можно задать
	// кегль мельче обычного, не вводя новый уровень в типографику.
	t := canvas.NewText(strings.ToUpper(text), TextMuted())
	t.TextSize = theme.TextSize() * 0.72
	t.TextStyle = fyne.TextStyle{Bold: true}
	t.Alignment = fyne.TextAlignLeading

	top := NavSectionLabelTopGap
	if top < 0 {
		top = 0
	}
	return container.New(&minHeight{min: NavSectionLabelHeight, max: NavSectionLabelHeight},
		container.New(&paddedBox{l: NavSectionLabelIndent, t: top, r: SidebarPadding, b: 0},
			container.NewCenter(container.NewHBox(t))))
}

// sidebarIdentityLabel — имя приложения.
//
// Имя продукта не локализуется, поэтому берётся из constants, а не через
// locale.T: оно же стоит в бандле и заголовке окна.
func sidebarIdentityLabel() fyne.CanvasObject {
	l := widget.NewLabel(constants.AppDisplayName)
	l.TextStyle = fyne.TextStyle{Bold: true}
	return l
}

// sidebarAppIcon — маркер приложения в шапке колонки.
//
// Компактная плашка-квадрат: настоящая иконка приложения живёт в бандле и в
// тему не переносится.
func sidebarAppIcon() fyne.CanvasObject {
	box := canvas.NewRectangle(Accent())
	box.CornerRadius = RadiusControl
	box.SetMinSize(fyne.NewSize(28, 28))
	txt := canvas.NewText("J", OnAccent())
	txt.TextStyle = fyne.TextStyle{Bold: true}
	txt.Alignment = fyne.TextAlignCenter
	return container.NewStack(box, container.NewCenter(txt))
}

// tintIcon перекрашивает themed-иконку в заданный цвет.
//
// Иконки у нас SVG с `currentColor`, поэтому тему наследуют автоматически.
// Чтобы выбранный пункт получил акцент, цвет нужно подменить: fyne.Theme
// умеет отдавать только один цвет на ресурс, а нам нужны два состояния у
// одной и той же иконки. nil-ресурс возвращается как есть.
func tintIcon(res fyne.Resource, c color.Color) fyne.Resource {
	if res == nil {
		return nil
	}
	return &tintResource{res: res, tint: c}
}

// --- Вспомогательные layout'ы ---------------------------------------------

// fixedHeight навязывает фиксированную высоту, сохраняя ширину.
type fixedHeight struct{ min float32 }

func (f *fixedHeight) Layout(objects []fyne.CanvasObject, size fyne.Size) {
	for _, o := range objects {
		o.Move(fyne.NewPos(0, 0))
		o.Resize(fyne.NewSize(size.Width, f.min))
	}
}

func (f *fixedHeight) MinSize(objects []fyne.CanvasObject) fyne.Size {
	var w float32
	for _, o := range objects {
		if m := o.MinSize().Width; m > w {
			w = m
		}
	}
	return fyne.NewSize(w, f.min)
}

// minHeight задаёт нижнюю границу высоты, позволяя содержимому вырасти до
// max (перенос длинной подписи на вторую строку). В отличие от fixedHeight
// не обрезает текст.
type minHeight struct{ min, max float32 }

func (m *minHeight) clamp(h float32) float32 {
	if h < m.min {
		h = m.min
	}
	if m.max > 0 && h > m.max {
		h = m.max
	}
	return h
}

func (m *minHeight) Layout(objects []fyne.CanvasObject, size fyne.Size) {
	h := m.clamp(size.Height)
	for _, o := range objects {
		o.Move(fyne.NewPos(0, 0))
		o.Resize(fyne.NewSize(size.Width, h))
	}
}

func (m *minHeight) MinSize(objects []fyne.CanvasObject) fyne.Size {
	var w, h float32
	for _, o := range objects {
		s := o.MinSize()
		if s.Width > w {
			w = s.Width
		}
		if s.Height > h {
			h = s.Height
		}
	}
	return fyne.NewSize(w, m.clamp(h))
}

// paddedBox — отступы по сторонам.
type paddedBox struct{ l, t, r, b float32 }

func (p *paddedBox) Layout(objects []fyne.CanvasObject, size fyne.Size) {
	w := size.Width - p.l - p.r
	h := size.Height - p.t - p.b
	if w < 0 {
		w = 0
	}
	if h < 0 {
		h = 0
	}
	for _, o := range objects {
		o.Move(fyne.NewPos(p.l, p.t))
		o.Resize(fyne.NewSize(w, h))
	}
}

func (p *paddedBox) MinSize(objects []fyne.CanvasObject) fyne.Size {
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
	return fyne.NewSize(min.Width+p.l+p.r, min.Height+p.t+p.b)
}
