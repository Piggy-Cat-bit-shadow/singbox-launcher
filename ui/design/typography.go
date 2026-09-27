// File typography.go — уровни шрифта для UI (SPEC 144).
//
// До SPEC 144 иерархия набиралась на месте: где-то
// `widget.NewLabelWithStyle(..., Bold)`, где-то `widget.NewLabel` плюс
// `Importance`, где-то размер вообще не задавался. Один и тот же по смыслу
// заголовок на разных экранах выглядел по-разному.
//
// Здесь зафиксировано пять уровней. Больше не нужно: страница, две ступени
// заголовков, основной текст и подпись покрывают все экраны лаунчера.
//
// **Почему не произвольные размеры.** Fyne сам выбирает кегль из темы
// (SizeNameText и т.д.) и умножает на масштаб. Задавать здесь абсолютные
// `fyne.NewSize(...)` значило бы подменить масштабирование темы своим и
// разойтись с системными настройками. Поэтому уровни различаются
// начертанием и ролью, а конкретный кегль берётся у темы.
package design

import (
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
)

// PageTitle — заголовок страницы в шапке ("Local", "Settings").
func PageTitle(text string) *canvas.Text {
	t := canvas.NewText(text, theme.ForegroundColor())
	t.TextStyle = fyne.TextStyle{Bold: true}
	t.TextSize = theme.TextSize() * 1.5
	t.Alignment = fyne.TextAlignLeading
	return t
}

// PageSubtitle — пояснение под заголовком страницы.
func PageSubtitle(text string) *canvas.Text {
	t := canvas.NewText(text, theme.Color(theme.ColorNameForeground))
	t.TextSize = theme.TextSize() * 0.92
	t.Alignment = fyne.TextAlignLeading
	// Подзаголовок приглушён относительно основного текста: он поясняет
	// заголовок, а не спорит с ним за внимание.
	t.Color = mutedForeground()
	return t
}

// SectionTitle — заголовок секции или карточки.
func SectionTitle(text string) *widget.Label {
	l := widget.NewLabel(text)
	l.TextStyle = fyne.TextStyle{Bold: true}
	l.Alignment = fyne.TextAlignLeading
	return l
}

// Body — основной текст.
func Body(text string) *widget.Label {
	l := widget.NewLabel(text)
	l.Alignment = fyne.TextAlignLeading
	return l
}

// BodyWrap — основной текст с переносом по словам. Для описаний, которые
// могут быть длиннее одной строки (подсказки, русская локализация).
func BodyWrap(text string) *widget.Label {
	l := Body(text)
	l.Wrapping = fyne.TextWrapWord
	return l
}

// Caption — мелкая подпись: метаданные, версия, адрес, пояснение к контролу.
func Caption(text string) *widget.Label {
	l := widget.NewLabel(text)
	l.TextStyle = fyne.TextStyle{}
	l.Importance = widget.LowImportance
	l.Alignment = fyne.TextAlignLeading
	return l
}

// CaptionWrap — мелкая подпись с переносом. Для описаний настроек.
func CaptionWrap(text string) *widget.Label {
	l := Caption(text)
	l.Wrapping = fyne.TextWrapWord
	return l
}

// mutedForeground — приглушённый цвет текста текущей темы.
//
// В Fyne нет отдельного semantic-цвета «вторичный текст»: есть foreground и
// disabled. Смешиваем foreground с фоном — так приглушение работает и в
// светлой, и в тёмной теме, в отличие от жёстко заданной серой константы.
func mutedForeground() color.Color {
	return blend(theme.ForegroundColor(), theme.Color(theme.ColorNameBackground), 0.62)
}
