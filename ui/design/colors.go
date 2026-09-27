// File colors.go — semantic-цвета поверх темы Fyne (SPEC 144).
//
// **Зачем слой поверх темы, а не своя палитра.** Тема Fyne уже даёт
// light/dark и корректно реагирует на смену системной темы. Своя палитра
// означала бы, что при переключении темы часть интерфейса остаётся в старых
// цветах: ровно тот дефект, которого избегаем.
//
// Поэтому здесь нет ни одного «красивого» hex-значения. Есть только
// семантические роли, каждая из которых выводится из цвета темы — либо
// напрямую, либо смешением. Смена темы автоматически перекрашивает всё.
//
// **Обновление при смене темы.** Цвета вычисляются в момент вызова, поэтому
// виджет, который их использует, обязан перечитать их в Refresh(). Для
// canvas-объектов это делается присваиванием FillColor/Color в Refresh.
//
// go1.20-совместимо (Win7-джоба): без slices/maps/min/max/clear.
package design

import (
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
)

// colorRGBA — рабочий тип для смешения; canvas-объекты принимают его как
// color.Color. Отдельный алиас не нужен, но объявлен, чтобы подпись
// blend() не выглядела как «магия» в местах вызова.
type colorRGBA = color.NRGBA

// variant — текущий вариант темы. Читается каждый раз заново: значение
// меняется при переключении light/dark, кэшировать его нельзя.
func variant() fyne.ThemeVariant {
	return fyne.CurrentApp().Settings().ThemeVariant()
}

// Background — фон окна (самый нижний слой).
func Background() color.Color {
	return theme.Color(theme.ColorNameBackground)
}

// Surface — поверхность карточки. Чуть отличается от фона: на этом различии
// и держится разделение слоёв, без теней.
func Surface() color.Color {
	return theme.Color(theme.ColorNameOverlayBackground)
}

// SurfaceHover — фон строки под курсором.
func SurfaceHover() color.Color {
	return blend(theme.Color(theme.ColorNameBackground), theme.Color(theme.ColorNamePrimary), 0.06)
}

// SurfaceSelected — фон выбранного пункта навигации или строки списка.
func SurfaceSelected() color.Color {
	return blend(theme.Color(theme.ColorNameBackground), theme.Color(theme.ColorNamePrimary), 0.16)
}

// Border — обычная граница: тонкая, низкоконтрастная. Ровно настолько,
// чтобы обозначить край поверхности, но не рисовать сетку.
func Border() color.Color {
	return blend(theme.Color(theme.ColorNameForeground), theme.Color(theme.ColorNameBackground), 0.82)
}

// BorderStrong — граница для акцентных элементов и разделителей, которым
// нужно быть заметнее обычных.
func BorderStrong() color.Color {
	return theme.Color(theme.ColorNameInputBorder)
}

// TextPrimary — основной текст.
func TextPrimary() color.Color {
	return theme.ForegroundColor()
}

// TextSecondary — вторичный текст: описания, метаданные.
func TextSecondary() color.Color {
	return mutedForeground()
}

// TextMuted — самый тихий текст: подписи под контролами, единицы измерения.
func TextMuted() color.Color {
	return blend(theme.ForegroundColor(), theme.Color(theme.ColorNameBackground), 0.45)
}

// Accent — акцентный цвет: выбранный пункт, активное состояние.
func Accent() color.Color {
	return theme.Color(theme.ColorNamePrimary)
}

// Success / Warning / Danger — статусные роли.
//
// В Fyne нет отдельных semantic-цветов для success/danger, поэтому берём
// то, что тема уже использует по смыслу: Success — цвет успеха, Warning —
// предупреждения, Danger — ошибки. Так статусные бейджи совпадают по цвету с
// системными диалогами и не выбиваются из темы.
func Success() color.Color { return theme.Color(theme.ColorNameSuccess) }
func Warning() color.Color { return theme.Color(theme.ColorNameWarning) }
func Danger() color.Color  { return theme.Color(theme.ColorNameError) }

// StatusColor — цвет статусного бейджа по уровню важности.
func StatusColor(level StatusLevel) color.Color {
	switch level {
	case StatusSuccess:
		return Success()
	case StatusWarning:
		return Warning()
	case StatusDanger:
		return Danger()
	case StatusInfo:
		return Accent()
	default:
		return TextMuted()
	}
}

// StatusLevel — уровень статусного бейджа.
type StatusLevel int

const (
	// StatusNeutral — нет выраженного состояния.
	StatusNeutral StatusLevel = iota
	// StatusInfo — информационное состояние (выбрано, в процессе).
	StatusInfo
	// StatusSuccess — успех (подключено, работает).
	StatusSuccess
	// StatusWarning — предупреждение (медленно, частично).
	StatusWarning
	// StatusDanger — ошибка (недоступно, таймаут).
	StatusDanger
)

// blend смешивает fg поверх bg с долей fgWeight (0 — только bg, 1 — только fg).
//
// Смешение нужно потому, что Fyne не даёт полупрозрачных слоёв поверх
// произвольного фона: alpha в NRGBA работает только если под ним уже что-то
// нарисовано. Непрозрачный результат предсказуем на любой платформе.
func blend(fg, bg color.Color, fgWeight float64) color.Color {
	if fgWeight < 0 {
		fgWeight = 0
	}
	if fgWeight > 1 {
		fgWeight = 1
	}
	fr, fg2, fb, fa := fg.RGBA()
	br, bg2, bb, ba := bg.RGBA()
	mix := func(f, b uint32) uint8 {
		return uint8((float64(f)*fgWeight+float64(b)*(1-fgWeight))/257) & 0xff
	}
	return color.NRGBA{R: mix(fr, br), G: mix(fg2, bg2), B: mix(fb, bb), A: mix(fa, ba)}
}
