// File page.go — шапка страницы и карточки (SPEC 144).
//
// **Зачем шапка отдельным компонентом.** До SPEC 144 у каждой страницы был
// свой способ начать текст: где-то `widget.NewLabel("Settings")`, где-то
// сразу список виджетов без заголовка вовсе. Пользователь не понимал, куда
// он попал, а заголовок страницы нельзя было найти поиском по коду.
//
// Шапка задаёт единый вертикальный ритм: заголовок, необязательный
// подзаголовок, необязательный статус справа. Высота ограничена
// PageHeaderHeight, чтобы шапка не съедала место у контента.
//
// go1.20-совместимо (Win7-джоба): без slices/maps/min/max/clear.
package design

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"
)

// PageHeader — шапка страницы: заголовок, подзаголовок и место под статус
// справа.
type PageHeader struct {
	object   fyne.CanvasObject
	trailing *fyne.Container
}

// NewPageHeader собирает шапку. trailing может быть nil — тогда правая часть
// пуста (место всё равно резервируется, чтобы заголовок не «прыгал» при
// появлении статуса).
func NewPageHeader(title, subtitle string, trailing fyne.CanvasObject) *PageHeader {
	titles := []fyne.CanvasObject{PageTitle(title)}
	if subtitle != "" {
		titles = append(titles, PageSubtitle(subtitle))
	}
	titleBox := container.NewVBox(titles...)

	if trailing == nil {
		trailing = canvas.NewRectangle(nil)
	}
	h := &PageHeader{trailing: container.NewCenter(trailing)}
	h.object = container.New(&paddedBox{l: ContentPaddingH, t: ContentPaddingV, r: ContentPaddingH, b: 0},
		container.NewBorder(nil, nil, nil, h.trailing, titleBox))
	return h
}

// Object возвращает объект для вставки в раскладку.
func (h *PageHeader) Object() fyne.CanvasObject { return h.object }

// SectionCard — карточка-секция: заголовок, необязательное описание и
// содержимое.
//
// **Почему поверхность, а не тень.** Тень в Fyne — отдельный canvas-объект,
// который перерисовывается при каждом изменении и по-разному выглядит на
// разных платформах и при дробном масштабе. Разделение слоёв здесь держится
// на трёх вещах: слегка другой фон, тонкая граница и радиус. Это дёшево и
// предсказуемо на всех трёх ОС.
type SectionCard struct {
	object fyne.CanvasObject
}

// NewSectionCard собирает карточку с заголовком и описанием (оба могут быть
// пустыми).
func NewSectionCard(title, description string, body fyne.CanvasObject) *SectionCard {
	items := make([]fyne.CanvasObject, 0, 3)
	if title != "" {
		items = append(items, SectionTitle(title))
	}
	if description != "" {
		items = append(items, CaptionWrap(description))
	}
	if len(items) > 0 && body != nil {
		items = append(items, canvas.NewRectangle(nil))
	}
	if body != nil {
		items = append(items, body)
	}

	inner := container.NewVBox(items...)

	bg := canvas.NewRectangle(Surface())
	bg.CornerRadius = RadiusCard
	border := canvas.NewRectangle(Border())
	border.CornerRadius = RadiusCard
	border.FillColor = nil
	border.StrokeColor = Border()
	border.StrokeWidth = 1

	c := &SectionCard{}
	c.object = container.NewStack(
		bg,
		border,
		container.New(&paddedBox{l: CardPadding, t: CardPadding, r: CardPadding, b: CardPadding}, inner),
	)
	return c
}

// Object возвращает объект карточки.
func (c *SectionCard) Object() fyne.CanvasObject { return c.object }

// StatusBadge — компактный статусный бейдж: точка-индикатор и подпись.
//
// Один компонент на все статусы приложения (Connected, Offline, Running,
// Updating, Error), чтобы одна и та же идея не выглядела по-разному на
// разных экранах.
type StatusBadge struct {
	object fyne.CanvasObject
	dot    *canvas.Circle
	label  *widget.Label
	level  StatusLevel
}

// NewStatusBadge создаёт бейдж с заданным уровнем.
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

// Set обновляет текст и уровень бейджа. Переиспользует существующие
// объекты — новых при обновлении статуса не создаётся.
func (b *StatusBadge) Set(text string, level StatusLevel) {
	b.level = level
	if b.label.Text != text {
		b.label.SetText(text)
	}
	b.dot.FillColor = StatusColor(level)
	b.dot.Refresh()
}

// fixedSizeBox — фиксированный бокс: точка-индикатор должна иметь стабильный
// размер независимо от метрик шрифта, иначе бейдж «дышит» при смене темы.
type fixedSizeBox struct {
	w, h float32
}

func (f *fixedSizeBox) Layout(objects []fyne.CanvasObject, _ fyne.Size) {
	for _, o := range objects {
		o.Move(fyne.NewPos(0, 0))
		o.Resize(fyne.NewSize(f.w, f.h))
	}
}

func (f *fixedSizeBox) MinSize([]fyne.CanvasObject) fyne.Size {
	return fyne.NewSize(f.w, f.h)
}

// HeaderTrailingBox — контейнер правой части шапки. Вынесен отдельно, чтобы
// страница могла подменять статус, не пересобирая шапку.
func HeaderTrailingBox() (*fyne.Container, fyne.CanvasObject) {
	c := container.NewHBox()
	return c, c
}
