// File card.go — карточки и строки карточек (SPEC 145).
//
// **Почему карточка — центральный элемент.** Современный десктопный клиент
// строится не из «полей формы», а из карточек: каждая группа данных живёт на
// собственной поверхности, отделённой от фона. Пока поверхность одна на всю
// страницу, интерфейс читается как таблица настроек.
//
// **Слои без теней.** Fyne не умеет размытую тень (это дорого и по-разному
// выглядит на разных ОС). Глубина делается тремя средствами: фон окна темнее
// поверхности, граница почти невидима, углы скруглены. Этого достаточно, и
// это предсказуемо при любом DPI.
//
// go1.20-совместимо (Win7-джоба): без slices/maps/min/max/clear.
package design

import (
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/widget"
	ttwidget "github.com/dweymouth/fyne-tooltip/widget"
)

// Card — карточка: заголовок, необязательное описание, тело.
type Card struct {
	object fyne.CanvasObject
	bg     *canvas.Rectangle
	border *canvas.Rectangle
	pad    *paddedBox
}

// NewCard собирает карточку.
//
// trailing — необязательный элемент в правой части заголовка (действие или
// шеврон). Тело может быть nil.
func NewCard(title, description string, trailing fyne.CanvasObject, body fyne.CanvasObject) *Card {
	items := make([]fyne.CanvasObject, 0, 4)
	if title != "" {
		head := container.NewHBox(CardTitle(title))
		if trailing != nil {
			head = container.NewBorder(nil, nil, nil, trailing, CardTitle(title))
		}
		items = append(items, head)
	}
	if description != "" {
		items = append(items, CardCaption(description))
	}
	if body != nil {
		if len(items) > 0 {
			items = append(items, vSpacer(SpaceM))
		}
		items = append(items, body)
	}

	c := &Card{}
	c.bg = canvas.NewRectangle(Surface())
	c.bg.CornerRadius = RadiusCard
	c.border = canvas.NewRectangle(color.Transparent)
	c.border.CornerRadius = RadiusCard
	c.border.StrokeColor = Border()
	c.border.StrokeWidth = 1

	c.pad = &paddedBox{l: CardPadding, t: CardPadding, r: CardPadding, b: CardPadding}
	c.object = container.NewStack(c.bg, c.border, container.New(c.pad, container.NewVBox(items...)))
	return c
}

// Object возвращает объект карточки.
func (c *Card) Object() fyne.CanvasObject { return c.object }

// Refresh перечитывает цвета темы. Вызывать при смене light/dark.
func (c *Card) Refresh() {
	c.bg.FillColor = Surface()
	c.border.StrokeColor = Border()
	c.bg.Refresh()
	c.border.Refresh()
}

// ClickableCard — карточка-ссылка: заголовок, сводка и шеврон справа.
//
// Вся карточка кликабельна, а не только заголовок: на десктопе попасть в
// строку целиком привычнее, чем в её текст.
type ClickableCard struct {
	widget.BaseWidget

	title    string
	summary  string
	onTapped func()

	bg     *canvas.Rectangle
	border *canvas.Rectangle
}

// NewClickableCard собирает карточку-ссылку.
func NewClickableCard(title, summary string, onTapped func()) *ClickableCard {
	c := &ClickableCard{title: title, summary: summary, onTapped: onTapped}
	c.ExtendBaseWidget(c)
	return c
}

// CreateRenderer implements fyne.Widget.
func (c *ClickableCard) CreateRenderer() fyne.WidgetRenderer {
	c.bg = canvas.NewRectangle(Surface())
	c.bg.CornerRadius = RadiusCard
	c.border = canvas.NewRectangle(color.Transparent)
	c.border.CornerRadius = RadiusCard
	c.border.StrokeColor = Border()
	c.border.StrokeWidth = 1

	head := container.NewBorder(nil, nil, nil, chevronRightIcon(), CardTitle(c.title))
	inner := []fyne.CanvasObject{head}
	if c.summary != "" {
		inner = append(inner, CardCaption(c.summary))
	}

	// Сводная карточка ниже обычной: внутри две строки текста, и большие
	// поля делали её похожей на пустую панель.
	content := container.New(&paddedBox{l: CardPadding, t: SpaceM, r: CardPadding, b: SpaceM},
		container.NewVBox(inner...))
	// Слои: фон → граница → содержимое. Прозрачного слоя сверху НЕТ.
	return widget.NewSimpleRenderer(container.NewStack(c.bg, c.border, content))
}

// ClickableCard сам обрабатывает и клик, и наведение.
//
// Отдельного прозрачного holder-слоя здесь СОЗНАТЕЛЬНО нет: слой поверх
// содержимого перехватывал бы попадания и делал недоступным всё, что лежит
// под ним. Реализация интерфейсов прямо на карточке даёт тот же эффект без
// overlay'я и без риска для вложенных контролов.
//
// Ограничение: внутри ClickableCard допустимы только текст, иконки и
// шеврон. Кнопки/поля/списки класть сюда нельзя — их события будет
// перехватывать карточка. Для таких случаев — обычный Card плюс отдельный
// интерактивный элемент.
var (
	_ fyne.Tappable     = (*ClickableCard)(nil)
	_ desktop.Hoverable = (*ClickableCard)(nil)
)

// setHover меняет заливку карточки под курсором.
func (c *ClickableCard) setHover(on bool) {
	if c.bg == nil {
		return
	}
	if on {
		c.bg.FillColor = SurfaceAlt()
	} else {
		c.bg.FillColor = Surface()
	}
	c.bg.Refresh()
}

// Tapped implements fyne.Tappable.
func (c *ClickableCard) Tapped(*fyne.PointEvent) {
	if c.onTapped != nil {
		c.onTapped()
	}
}

// MouseIn implements desktop.Hoverable.
func (c *ClickableCard) MouseIn(*desktop.MouseEvent) { c.setHover(true) }

// MouseMoved implements desktop.Hoverable.
func (c *ClickableCard) MouseMoved(*desktop.MouseEvent) {}

// MouseOut implements desktop.Hoverable.
func (c *ClickableCard) MouseOut() { c.setHover(false) }

// CardRow — строка внутри карточки: заголовок, описание и значение/действие
// справа. Может быть кликабельной.
type CardRow struct {
	object   fyne.CanvasObject
	bg       *canvas.Rectangle
	title    *widget.Label
	subtitle *ttwidget.Label
	onTapped func()
	holder   *rowHoverHolder
	selected bool
}

// NewCardRow собирает строку.
func NewCardRow(title, subtitle string, trailing fyne.CanvasObject, onTapped func()) *CardRow {
	r := &CardRow{onTapped: onTapped}
	r.bg = canvas.NewRectangle(color.Transparent)
	r.bg.CornerRadius = RadiusButton
	r.bg.Hide()

	r.title = widget.NewLabel(title)
	r.title.Truncation = fyne.TextTruncateEllipsis

	text := container.NewVBox(r.title)
	if subtitle != "" {
		r.subtitle = CardCaption(subtitle)
		text.Add(r.subtitle)
	}

	var body fyne.CanvasObject = container.NewBorder(nil, nil, nil, trailing, text)
	inner := container.New(&paddedBox{l: SpaceM, t: SpaceS, r: SpaceM, b: SpaceS}, body)

	// Правило наложения (SPEC 145 fix-wave §10).
	//
	// Прозрачный holder, положенный ВЕРХНИМ слоем, перехватывает попадания и
	// делает недоступными кнопки/поля, лежащие под ним. Поэтому holder
	// добавляется только тогда, когда в строке НЕТ интерактивного trailing:
	// иначе клик по вложенному контролу не доходил бы до него.
	//
	// Это не теоретический риск: в строки настроек и диагностики кладут
	// Button/Check/Select/Entry, и обёртка сверху ломала бы именно их.
	stack := container.NewStack(r.bg, inner)
	if onTapped != nil && !trailingInteractive(trailing) {
		r.holder = &rowHoverHolder{row: r}
		r.holder.ExtendBaseWidget(r.holder)
		stack = container.NewStack(stack, r.holder)
	}
	r.object = container.New(&minHeight{min: RowMinHeight, max: 0}, stack)
	return r
}

// trailingInteractive сообщает, является ли правая часть строки интерактивным
// контролом.
//
// Проверяем по интерфейсам, а не по конкретным типам: любой виджет, умеющий
// принимать тап или нажатие клавиши, не должен оказаться под прозрачным
// holder'ом.
func trailingInteractive(trailing fyne.CanvasObject) bool {
	if trailing == nil {
		return false
	}
	switch trailing.(type) {
	case fyne.Tappable, fyne.Focusable, desktop.Hoverable:
		return true
	}
	// Контейнеры (HBox/VBox/Border) сами не Tappable, но могут содержать
	// кнопку. Консервативно считаем интерактивным всё, что не распознано как
	// простой текст/иконка.
	switch trailing.(type) {
	case *widget.Label, *canvas.Text, *widget.Icon, *canvas.Rectangle, *canvas.Circle:
		return false
	}
	return true
}

// Object возвращает объект строки.
func (r *CardRow) Object() fyne.CanvasObject { return r.object }

// SetSubtitle обновляет описание (для строк со сменными данными).
func (r *CardRow) SetSubtitle(text string) {
	if r.subtitle == nil {
		return
	}
	r.subtitle.SetText(text)
}

// rowHoverHolder перехватывает наведение и клик по строке.
type rowHoverHolder struct {
	widget.BaseWidget
	row *CardRow
}

// CreateRenderer implements fyne.Widget.
func (h *rowHoverHolder) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(canvas.NewRectangle(color.Transparent))
}

// Tapped implements fyne.Tappable.
func (h *rowHoverHolder) Tapped(*fyne.PointEvent) {
	if h.row.onTapped != nil {
		h.row.onTapped()
	}
}

// MouseIn implements desktop.Hoverable.
func (h *rowHoverHolder) MouseIn(*desktop.MouseEvent) {
	h.row.bg.FillColor = SurfaceHover()
	h.row.bg.Show()
	h.row.bg.Refresh()
}

// MouseMoved implements desktop.Hoverable.
func (h *rowHoverHolder) MouseMoved(*desktop.MouseEvent) {}

// MouseOut implements desktop.Hoverable.
func (h *rowHoverHolder) MouseOut() {
	h.row.bg.Hide()
	h.row.bg.Refresh()
}

// chevronRightIcon — шеврон «перейти» в правой части кликабельной карточки.
func chevronRightIcon() fyne.CanvasObject {
	return widget.NewIcon(tintIcon(ChevronRightResource(), TextMuted()))
}

// vSpacer — вертикальный распор.
func vSpacer(h float32) fyne.CanvasObject {
	r := canvas.NewRectangle(color.Transparent)
	r.SetMinSize(fyne.NewSize(1, h))
	return r
}

// CardTitle — заголовок карточки.
func CardTitle(text string) *widget.Label {
	l := widget.NewLabel(text)
	l.TextStyle = fyne.TextStyle{Bold: true}
	return l
}

// CardCaption — вторичный текст карточки: описание или сводка.
func CardCaption(text string) *ttwidget.Label {
	l := ttwidget.NewLabel(text)
	l.Wrapping = fyne.TextWrapWord
	l.Importance = widget.LowImportance
	return l
}

// ChevronRightResource — шеврон для карточек и строк.
//
// Обёртка над иконкой набора, чтобы design не зависел от пакета icons
// напрямую: набор иконок — ресурс приложения, а не часть дизайн-системы.
// Значение подставляется при инициализации пакета ui.
var ChevronRightResource = func() fyne.Resource { return nil }

// SetChevronResource связывает дизайн-систему с набором иконок приложения.
// Вызывается один раз при сборке UI.
func SetChevronResource(r fyne.Resource) { ChevronRightResource = func() fyne.Resource { return r } }
