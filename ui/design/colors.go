// File colors.go — доступ к семантическим цветам (SPEC 145).
//
// Раньше (SPEC 144) цвета выводились смешением цветов дефолтной темы Fyne.
// Теперь источник — палитра (`palette.go`), а этот файл лишь даёт удобный
// доступ к ролям для компонентов.
//
// **Правило жизненного цикла.** Функции читают ТЕКУЩИЙ вариант темы при
// каждом вызове и возвращают значение. Компонент обязан вызывать их в
// `Refresh()`, а не кэшировать результат в поле: иначе при переключении
// light/dark пользовательские canvas-объекты останутся в старом цвете, пока
// стандартные виджеты уже перекрасятся.
//
// go1.20-совместимо (Win7-джоба): без slices/maps/min/max/clear.
package design

import (
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
)

// currentPalette — палитра активного варианта темы.
func currentPalette() Palette {
	return PaletteFor(currentVariant())
}

// currentVariant — вариант темы приложения. Читается каждый раз: значение
// меняется при переключении системной темы, кэшировать нельзя. nil-приложение
// (модульные тесты без Fyne) даёт светлую палитру вместо паники.
func currentVariant() fyne.ThemeVariant {
	app := fyne.CurrentApp()
	if app == nil {
		// Вне приложения (модульные тесты) считаем светлую тему: она
		// безопаснее как значение по умолчанию.
		return theme.VariantLight
	}
	return app.Settings().ThemeVariant()
}

// Background — фон окна.
func Background() color.Color { return currentPalette().Background }

// SidebarSurface — фон навигационной колонки (отличается от Background).
func SidebarSurface() color.Color { return currentPalette().Sidebar }

// Surface — поверхность карточек.
func Surface() color.Color { return currentPalette().Surface }

// SurfaceAlt — второстепенная поверхность: поля ввода, вложенные блоки.
func SurfaceAlt() color.Color { return currentPalette().SurfaceAlt }

// SurfaceHover — фон под курсором.
func SurfaceHover() color.Color { return currentPalette().SurfaceHover }

// SurfaceSelected — мягкая заливка выбранного пункта.
func SurfaceSelected() color.Color { return currentPalette().SurfaceSelected }

// Border — обычная граница.
func Border() color.Color { return currentPalette().Border }

// BorderStrong — граница полей ввода.
func BorderStrong() color.Color { return currentPalette().BorderStrong }

// TextPrimary — основной текст.
func TextPrimary() color.Color { return currentPalette().TextPrimary }

// TextSecondary — описания и подписи.
func TextSecondary() color.Color { return currentPalette().TextSecondary }

// TextMuted — самый тихий текст.
func TextMuted() color.Color { return currentPalette().TextMuted }

// Accent — акцентный цвет.
func Accent() color.Color { return currentPalette().Primary }

// AccentHover — акцент под курсором.
func AccentHover() color.Color { return currentPalette().PrimaryHover }

// OnAccent — текст на акцентной заливке.
func OnAccent() color.Color { return currentPalette().OnPrimary }

// Success / Warning / Danger — статусы.
func Success() color.Color { return currentPalette().Success }
func Warning() color.Color { return currentPalette().Warning }
func Danger() color.Color  { return currentPalette().Danger }

// Shadow — почти прозрачная подложка под карточками.
func Shadow() color.Color { return currentPalette().Shadow }

// StatusColor — цвет статусного бейджа по уровню.
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
	// StatusInfo — информационное состояние.
	StatusInfo
	// StatusSuccess — успех (подключено, работает).
	StatusSuccess
	// StatusWarning — предупреждение (медленно, частично).
	StatusWarning
	// StatusDanger — ошибка (недоступно, таймаут).
	StatusDanger
)

// LatencyColor — цвет индикатора задержки по её значению в миллисекундах.
//
// Пороги — визуальные, не бизнес-правила: они не влияют ни на выбор узла,
// ни на что-либо в core. Отрицательное значение означает «нет измерения»
// (таймаут или узел не проверялся).
func LatencyColor(ms int) color.Color {
	switch {
	case ms < 0:
		return TextMuted()
	case ms < 100:
		return Success()
	case ms < 250:
		return TextSecondary()
	case ms < 500:
		return Warning()
	default:
		return Danger()
	}
}
