// File sidebar.go — вертикальная навигация главного окна (SPEC 144).
//
// **Что заменяется.** До SPEC 144 главной навигацией был `container.AppTabs`
// с emoji-заголовками. Горизонтальный таб-стрип ограничен шириной окна и не
// умеет иерархию: все страницы — соседи одного уровня, а подразделы Settings
// или Diagnostics вообще невидимы. Сайдбар снимает оба ограничения.
//
// **Почему композиция, а не свой Renderer.** `BaseWidget` + `Renderer` дал бы
// контроль над раскладкой ценой ручного MinSize/Layout/Refresh, где ошибка
// проявляется как схлопнувшаяся панель или пропавший при смене темы цвет.
// Здесь всё собрано из стандартных контейнеров и одного `canvas.Rectangle`
// на фон; перекраска при смене темы — присваивание FillColor в Refresh.
//
// **Состояние.** Сайдбар знает только о навигации: какой пункт выбран и
// какие родители развёрнуты. Он НЕ знает, запущено ли ядро, выбрана ли
// машина, подключён ли Remote — это прерогатива AppController и EventBus.
// Иначе появилась бы вторая копия истины (§84 SPEC-требований).
//
// go1.20-совместимо (Win7-джоба): без slices/maps/min/max/clear.
package design

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"singbox-launcher/internal/constants"
	"singbox-launcher/internal/fynewidget"
	"singbox-launcher/ui/icons"
)

// SidebarItemID — стабильный идентификатор пункта навигации.
type SidebarItemID string

// SidebarEntry — один пункт навигации: либо страница, либо родитель
// с подпунктами.
type SidebarEntry struct {
	// ID — идентификатор страницы. Для родителя с детьми не используется
	// как цель навигации (клик по нему только разворачивает).
	ID SidebarItemID
	// Title — подпись пункта.
	Title string
	// Icon — иконка первого уровня. У подпунктов обычно nil.
	Icon fyne.Resource
	// Children — подпункты. Пусто — пункт ведёт на страницу сам.
	Children []SidebarEntry
	// Section — заголовок группы над пунктом (необязательно). Показывается
	// только у пунктов верхнего уровня.
	Section string
}

// Sidebar — навигационная колонка. Собирается из SidebarEntry и сообщает о
// выборе через onSelect. Никакой бизнес-логики внутри.
type Sidebar struct {
	widget.BaseWidget

	entries  []SidebarEntry
	onSelect func(SidebarItemID)

	// selected — текущий активный пункт (может быть подпунктом).
	selected SidebarItemID
	// expanded — развёрнутые родители. Только в памяти: SPEC 144 §9.4 —
	// ради одного chevron не заводим новый диск-схема-ключ.
	expanded map[SidebarItemID]bool

	// rows — построенные строки для перерисовки состояния. Ключ — ID.
	rows map[SidebarItemID]*sidebarRow
	// order — порядок строк в колонке (родитель, затем его дети).
	order []SidebarItemID

	body *fyne.Container
	// dot / statusLabel — нижняя строка со статусом ядра.
	dot         *canvas.Circle
	statusLabel *widget.Label
}

// maximal — верхняя граница: сайдбар не должен тянуться по ширине.
var _ fyne.Widget = (*Sidebar)(nil)

// NewSidebar собирает колонку навигации.
//
// onSelect вызывается на UI-потоке при клике по пункту или подпункту. Клик
// по родителю с детьми НЕ вызывает onSelect: он только разворачивает список
// и не меняет активную страницу (SPEC 144 §9.3).
func NewSidebar(entries []SidebarEntry, onSelect func(SidebarItemID)) *Sidebar {
	s := &Sidebar{
		entries:  entries,
		onSelect: onSelect,
		expanded: make(map[SidebarItemID]bool),
		rows:     make(map[SidebarItemID]*sidebarRow),
	}
	s.ExtendBaseWidget(s)
	s.build()
	return s
}

// SetSelected помечает пункт активным и разворачивает его родителя, чтобы
// активный подпункт был виден.
func (s *Sidebar) SetSelected(id SidebarItemID) {
	s.selected = id
	// Активный ребёнок обязан быть видим: иначе после перезапуска или
	// программной навигации выбор окажется внутри свёрнутой группы.
	for _, e := range s.entries {
		for _, c := range e.Children {
			if c.ID == id {
				s.expanded[e.ID] = true
			}
		}
	}
	s.applyVisualState()
}

// Selected возвращает активный пункт.
func (s *Sidebar) Selected() SidebarItemID { return s.selected }

// IsExpanded сообщает, развёрнут ли родитель.
func (s *Sidebar) IsExpanded(id SidebarItemID) bool { return s.expanded[id] }

// build конструирует строки один раз. Дальнейшие изменения состояния только
// перекрашивают уже созданные объекты — новые CanvasObject при клике не
// создаются (требование по производительности, SPEC 144 §47/§61).
func (s *Sidebar) build() {
	objs := make([]fyne.CanvasObject, 0, len(s.entries)*3)
	var lastSection string

	for _, e := range s.entries {
		if e.Section != "" && e.Section != lastSection {
			objs = append(objs, sidebarSectionLabel(e.Section))
			lastSection = e.Section
		}
		row := newSidebarRow(e, s)
		s.rows[e.ID] = row
		s.order = append(s.order, e.ID)
		objs = append(objs, row.object)

		for _, c := range e.Children {
			crow := newSidebarRow(c, s)
			crow.isChild = true
			crow.parent = e.ID
			s.rows[c.ID] = crow
			s.order = append(s.order, c.ID)
			objs = append(objs, crow.object)
		}
	}

	s.body = container.NewVBox(objs...)
	s.applyVisualState()
}

// applyVisualState перекрашивает строки под текущее selected/expanded.
func (s *Sidebar) applyVisualState() {
	for _, id := range s.order {
		row := s.rows[id]
		if row == nil {
			continue
		}
		// Родитель подсвечен и тогда, когда активен его подпункт: свёрнутая
		// группа не должна терять признак «ты сейчас здесь» (§9.3).
		active := id == s.selected
		if !active && len(row.entry.Children) > 0 {
			for _, c := range row.entry.Children {
				if c.ID == s.selected {
					active = true
					break
				}
			}
		}
		row.setActive(active)
		row.setExpanded(s.expanded[id])
		if row.isChild {
			row.setVisible(s.expanded[row.parent])
		}
	}
}

// toggle разворачивает/сворачивает родителя. Активная страница при этом не
// меняется — метод не трогает s.selected.
func (s *Sidebar) toggle(id SidebarItemID) {
	s.expanded[id] = !s.expanded[id]
	s.applyVisualState()
}

// selectItem — клик по导航 пункту.
func (s *Sidebar) selectItem(e SidebarEntry) {
	if len(e.Children) > 0 {
		s.toggle(e.ID)
		return
	}
	if e.ID == s.selected {
		return
	}
	s.SetSelected(e.ID)
	if s.onSelect != nil {
		s.onSelect(e.ID)
	}
}

// CreateRenderer implements fyne.Widget.
func (s *Sidebar) CreateRenderer() fyne.WidgetRenderer {
	bg := canvas.NewRectangle(theme.Color(theme.ColorNameBackground))
	// Очень тонкая граница между навигацией и контентом: разделяет слои, не
	// превращаясь в рамку.
	edge := canvas.NewRectangle(Border())
	edge.SetMinSize(fyne.NewSize(1, 0))

	navigation := container.NewVBox(
		container.NewCenter(container.NewVBox(
			container.New(&fixedHeight{min: SidebarIdentityHeight},
				container.NewCenter(sidebarIdentityLabel())),
		)),
		container.New(&paddedBox{l: SidebarPadding, t: 0, r: SidebarPadding, b: SidebarPadding}, s.body),
	)

	// Статус — внизу колонки. Раньше состояние ядра показывал emoji в
	// заголовке вкладки Local (▶️/⏸️); в сайдбаре для него есть отдельное
	// место, и emoji там больше не нужен.
	content := container.NewBorder(nil, s.footer(), nil, edge, navigation)
	return widget.NewSimpleRenderer(container.NewStack(bg, content))
}

// footer — нижняя строка сайдбара со статусом ядра.
func (s *Sidebar) footer() fyne.CanvasObject {
	s.dot = canvas.NewCircle(StatusColor(StatusNeutral))
	dotBox := container.New(&fixedSizeBox{w: 8, h: 8}, s.dot)
	s.statusLabel = widget.NewLabel("—")
	s.statusLabel.Importance = widget.LowImportance
	s.statusLabel.Truncation = fyne.TextTruncateEllipsis
	row := container.NewHBox(container.NewCenter(dotBox), s.statusLabel)
	return container.New(&paddedBox{l: SidebarPadding, t: SpaceS, r: SidebarPadding, b: SpaceM},
		container.New(&fixedHeight{min: NavItemHeight}, container.NewCenter(row)))
}

// SetStatus обновляет статус ядра в нижней строке колонки.
//
// Это presentation-состояние: источник истины по-прежнему один —
// AppController.RunningState, а сюда лишь передаётся его проекция. Второй
// копии «подключено/нет» здесь не заводится.
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

// MinSize задаёт ширину колонки и не даёт содержимому её раздуть:
// длинный пункт обрезается, но не расширяет сайдбар.
func (s *Sidebar) MinSize() fyne.Size {
	return fyne.NewSize(SidebarWidth, s.body.MinSize().Height+SidebarIdentityHeight+SidebarTopGap)
}

// sidebarIdentityLabel — название приложения в шапке колонки.
//
// Имя берётся из constants.AppDisplayName, а не пишется строкой: это имя
// продукта (оно же в бандле и заголовке окна), и оно не локализуется —
// поэтому через locale.T оно не идёт.
func sidebarIdentityLabel() fyne.CanvasObject {
	l := widget.NewLabel(constants.AppDisplayName)
	l.TextStyle = fyne.TextStyle{Bold: true}
	return l
}

// sidebarSectionLabel — заголовок группы пунктов.
func sidebarSectionLabel(text string) fyne.CanvasObject {
	l := widget.NewLabel(text)
	l.Importance = widget.LowImportance
	return container.New(&paddedBox{l: SidebarPadding + NavSubIndent, t: SpaceS, r: SidebarPadding, b: SpaceXS}, l)
}

// --- Строка пункта ---------------------------------------------------------

// sidebarRow — одна строка навигации: фон, акцентная полоса, иконка, текст и
// (для родителя) шеврон.
type sidebarRow struct {
	entry   SidebarEntry
	sidebar *Sidebar

	object fyne.CanvasObject
	bg     *canvas.Rectangle
	strip  *canvas.Rectangle
	label  *widget.Label
	icon   *widget.Icon
	chev   *widget.Icon

	active   bool
	expanded bool
	isChild  bool
	parent   SidebarItemID

	holder *sidebarRowHolder
}

// sidebarRowHolder перехватывает клики и наведение на строку.
type sidebarRowHolder struct {
	widget.BaseWidget
	row    *sidebarRow
	object fyne.CanvasObject
}

func newSidebarRow(e SidebarEntry, s *Sidebar) *sidebarRow {
	r := &sidebarRow{entry: e, sidebar: s}

	// Фон и акцентная полоса лежат в Stack ПОД содержимым. Обе создаются
	// один раз; при наведении/выборе меняется только их цвет.
	r.bg = canvas.NewRectangle(nil)
	r.bg.CornerRadius = RadiusButton
	r.bg.Hide()
	r.strip = canvas.NewRectangle(Accent())
	r.strip.CornerRadius = NavSelectedIndicator / 2
	r.strip.Hide()

	r.label = widget.NewLabel(e.Title)
	r.label.Truncation = fyne.TextTruncateEllipsis
	r.label.Alignment = fyne.TextAlignLeading

	// Иконка первого уровня крупнее подпункта: так вес уровней читается без
	// дополнительных линий и отступов.
	iconSize := NavIconSize
	if len(e.Children) == 0 && e.Icon == nil {
		iconSize = NavSubIconSize
	}
	if e.Icon != nil {
		r.icon = widget.NewIcon(e.Icon)
		r.icon.Resize(fyne.NewSize(iconSize, iconSize))
	} else {
		// Пустая прокладка сохраняет выравнивание текста подпункта с
		// текстом родителя.
		r.icon = widget.NewIcon(nil)
	}

	left := container.NewHBox(r.icon, r.label)
	// Трейлинг: у родителя — шеврон, у листа — ничего. Пустой placeholder
	// держит одинаковую геометрию, чтобы клик по строке не менял раскладку.
	var trailing fyne.CanvasObject = canvas.NewRectangle(nil)
	if len(e.Children) > 0 {
		r.chev = widget.NewIcon(chevronResource(false))
		trailing = r.chev
	}

	body := container.NewBorder(nil, nil, left, trailing)

	holder := &sidebarRowHolder{row: r}
	holder.ExtendBaseWidget(holder)
	r.holder = holder

	stack := container.NewStack(r.bg, r.strip, container.New(&paddedBox{l: SidebarPadding, t: 0, r: SidebarPadding, b: 0}, body))
	if r.isChild {
		// Подпункт сдвинут вправо — иерархия видна и без цветовых подсказок.
		stack = container.NewStack(r.bg, r.strip, container.New(&paddedBox{l: SidebarPadding + NavSubIndent, t: 0, r: SidebarPadding, b: 0}, body))
	}
	holder.object = container.NewStack(stack, holder)
	r.object = container.New(&fixedHeight{min: rowHeightFor(e)}, holder.object)
	fynewidget.SetToolTipSafe(r.object, e.Title)
	return r
}

func rowHeightFor(e SidebarEntry) float32 {
	if e.Icon == nil && len(e.Children) == 0 {
		return NavSubItemHeight
	}
	return NavItemHeight
}

// setActive переключает подсветку выбранного пункта.
func (r *sidebarRow) setActive(active bool) {
	if r.active == active && r.bg.Visible() == active {
		return
	}
	r.active = active
	if active {
		r.bg.FillColor = SurfaceSelected()
		r.bg.Show()
		r.strip.Show()
		r.label.TextStyle = fyne.TextStyle{Bold: true}
	} else {
		r.bg.Hide()
		r.strip.Hide()
		r.label.TextStyle = fyne.TextStyle{}
	}
	r.label.Refresh()
}

// setExpanded обновляет шеврон родителя.
func (r *sidebarRow) setExpanded(expanded bool) {
	if r.chev == nil {
		return
	}
	r.expanded = expanded
	r.chev.Resource = chevronResource(expanded)
	r.chev.Refresh()
}

// setVisible скрывает/показывает подпункт вместе с родителем.
func (r *sidebarRow) setVisible(visible bool) {
	if visible {
		r.object.Show()
		return
	}
	r.object.Hide()
}

// applyHover перекрашивает фон под курсором. Выбранный пункт сохраняет свой
// цвет: hover не должен «гасить» активное состояние.
func (r *sidebarRow) applyHover(hovered bool) {
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

// CreateRenderer implements fyne.Widget for the click/hover holder.
func (h *sidebarRowHolder) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(canvas.NewRectangle(nil))
}

// Tapped implements fyne.Tappable.
func (h *sidebarRowHolder) Tapped(*fyne.PointEvent) {
	h.row.sidebar.selectItem(h.row.entry)
}

// MouseIn implements desktop.Hoverable.
func (h *sidebarRowHolder) MouseIn(*desktop.MouseEvent) { h.row.applyHover(true) }

// MouseMoved implements desktop.Hoverable.
func (h *sidebarRowHolder) MouseMoved(*desktop.MouseEvent) {}

// MouseOut implements desktop.Hoverable.
func (h *sidebarRowHolder) MouseOut() { h.row.applyHover(false) }

func chevronResource(expanded bool) fyne.Resource {
	if expanded {
		return icons.ChevronDown
	}
	return icons.ChevronRight
}

// --- Вспомогательные layout'ы ---------------------------------------------

// fixedHeight навязывает содержимому фиксированную высоту, сохраняя ширину
// родителя. Нужен, чтобы строка навигации имела ровный вертикальный ритм
// независимо от метрик шрифта конкретной ОС.
type fixedHeight struct {
	min float32
}

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

// paddedBox — отступы по сторонам без создания вложенных контейнеров.
type paddedBox struct {
	l, t, r, b float32
}

func (p *paddedBox) Layout(objects []fyne.CanvasObject, size fyne.Size) {
	for _, o := range objects {
		o.Move(fyne.NewPos(p.l, p.t))
		o.Resize(fyne.NewSize(size.Width-p.l-p.r, size.Height-p.t-p.b))
	}
}

func (p *paddedBox) MinSize(objects []fyne.CanvasObject) fyne.Size {
	var min fyne.Size
	for _, o := range objects {
		if m := o.MinSize(); m.Width > min.Width || m.Height > min.Height {
			if m.Width > min.Width {
				min.Width = m.Width
			}
			if m.Height > min.Height {
				min.Height = m.Height
			}
		}
	}
	return fyne.NewSize(min.Width+p.l+p.r, min.Height+p.t+p.b)
}
