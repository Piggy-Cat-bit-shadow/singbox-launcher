// File theme.go — глобальная тема приложения (SPEC 145).
//
// **Зачем своя тема.** Fyne рисует стандартные контролы (кнопки, поля,
// чекбоксы, скроллбары, разделители) целиком из `fyne.Theme`. Пока тема
// дефолтная, любой layout остаётся узнаваемо «финевым»: серые кнопки,
// прямоугольные поля, резкие границы. Переопределив `Color()` и `Size()`,
// мы меняем внешний вид всех стандартных виджетов сразу — без переписывания
// каждого и без своих renderer'ов.
//
// **Что НЕ переопределяется.** `Font()` и `Icon()` делегируют дефолтной теме:
// подмена шрифтов сломала бы метрики на Windows/Linux и сломала бы встроенный
// набор иконок. Иконки навигации у нас свои (SVG), иконки контролов — темы.
//
// **Почему Size(), а не только Color().** Половина «финевости» — в размерах:
// радиусы скруглений и толщина разделителей по умолчанию малы. Радиусы
// задаются здесь, поэтому все стандартные виджеты получают современное
// скругление согласованно.
//
// go1.20-совместимо (Win7-джоба): без slices/maps/min/max/clear.
package design

import (
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
)

// Варианты темы берутся ТОЛЬКО из fyne/theme. Своих числовых значений здесь
// быть не должно: VariantDark == 0 (iota), VariantLight == 1. Рукописные
// алиасы вида fyne.ThemeVariant(1)/ThemeVariant(2) однажды уже привели к
// тому, что светлая системная тема (Light == 1) попадала в тёмную палитру —
// окно рисовалось тёмным под светлым системным titlebar.
//
// AppearanceMode — пользовательский выбор внешнего вида.
type AppearanceMode string

const (
	// AppearanceSystem — следовать системной теме (по умолчанию).
	AppearanceSystem AppearanceMode = "system"
	// AppearanceLight — принудительно светлая.
	AppearanceLight AppearanceMode = "light"
	// AppearanceDark — принудительно тёмная.
	AppearanceDark AppearanceMode = "dark"
)

// Theme — тема приложения. Реализует fyne.Theme.
type Theme struct {
	base fyne.Theme
	// mode — выбранный пользователем внешний вид. Пустое значение означает
	// System: так тема ведёт себя правильно даже если её собрали без
	// явного указания режима.
	mode AppearanceMode
}

// NewTheme собирает тему поверх дефолтной. Дефолтная нужна для шрифтов и
// иконок, которые мы сознательно не подменяем.
//
// mode — System/Light/Dark; неизвестное значение трактуется как System.
func NewTheme(mode AppearanceMode) *Theme {
	return &Theme{base: theme.DefaultTheme(), mode: normalizeAppearance(mode)}
}

// normalizeAppearance приводит значение к одному из трёх допустимых.
func normalizeAppearance(m AppearanceMode) AppearanceMode {
	switch m {
	case AppearanceLight, AppearanceDark:
		return m
	default:
		return AppearanceSystem
	}
}

// SetMode меняет режим внешнего вида и перерисовывает всё дерево.
//
// Вызывается из настроек: Fyne не имеет публичного SetThemeVariant, поэтому
// вариант выбирается внутри темы, а не через Environment/private API.
func (t *Theme) SetMode(mode AppearanceMode) {
	t.mode = normalizeAppearance(mode)
	if app := fyne.CurrentApp(); app != nil {
		// Refresh заставляет все виджеты перечитать цвета из темы.
		app.Settings().SetTheme(t)
	}
}

// Mode возвращает текущий режим.
func (t *Theme) Mode() AppearanceMode { return t.mode }

// effectiveVariant приводит запрошенный системой вариант к выбранному режиму.
//
// В режиме System возвращается системный вариант как есть — поэтому на macOS
// содержимое окна совпадает с нативным titlebar. В Light/Dark вариант
// подменяется, но native titlebar остаётся системным: публичного способа
// переключить оформление окна в Fyne нет, и приватные AppKit-вызовы здесь
// сознательно не используются (см. docs/release_notes).
func (t *Theme) effectiveVariant(requested fyne.ThemeVariant) fyne.ThemeVariant {
	switch t.mode {
	case AppearanceLight:
		return theme.VariantLight
	case AppearanceDark:
		return theme.VariantDark
	default:
		return requested
	}
}

var _ fyne.Theme = (*Theme)(nil)

// Color возвращает цвет роли для варианта темы.
//
// Таблица соответствий собрана по факту использования в стандартных виджетах
// Fyne: например, фон окна — ColorNameBackground, поверхность карточек и
// поповеров — ColorNameOverlayBackground, поля ввода — ColorNameInputBackground.
// Неизвестные роли отдаются дефолтной теме: так новая версия Fyne, добавившая
// цвет, не останется без значения.
func (t *Theme) Color(name fyne.ThemeColorName, v fyne.ThemeVariant) color.Color {
	p := PaletteFor(t.effectiveVariant(v))
	switch name {
	case theme.ColorNameBackground:
		return p.Background
	case theme.ColorNameOverlayBackground:
		// Поповеры, меню, диалоги: самый «приподнятый» слой.
		return p.Surface
	case theme.ColorNameMenuBackground:
		return p.Surface
	case theme.ColorNameHeaderBackground:
		return p.Surface

	case theme.ColorNameInputBackground:
		return p.SurfaceAlt
	case theme.ColorNameInputBorder:
		return p.BorderStrong
	case theme.ColorNameButton:
		return p.SurfaceAlt
	case theme.ColorNameDisabledButton:
		return p.SurfaceAlt

	case theme.ColorNameForeground:
		return p.TextPrimary
	case theme.ColorNamePlaceHolder:
		return p.TextMuted
	case theme.ColorNameDisabled:
		return p.TextMuted
	case theme.ColorNameHyperlink:
		return p.Primary

	case theme.ColorNamePrimary:
		return p.Primary
	case theme.ColorNameForegroundOnPrimary:
		return p.OnPrimary
	case theme.ColorNameSuccess:
		return p.Success
	case theme.ColorNameForegroundOnSuccess:
		// На зелёной заливке читается только белый.
		return color.NRGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF}
	case theme.ColorNameWarning:
		return p.Warning
	case theme.ColorNameForegroundOnWarning:
		return color.NRGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF}
	case theme.ColorNameError:
		return p.Danger
	case theme.ColorNameForegroundOnError:
		return color.NRGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF}

	case theme.ColorNameHover:
		return p.SurfaceHover
	case theme.ColorNamePressed:
		return p.SurfaceHover
	case theme.ColorNameFocus:
		return p.Focus
	case theme.ColorNameSelection:
		return p.SurfaceSelected
	case theme.ColorNameSeparator:
		return p.Border
	case theme.ColorNameShadow:
		return p.Shadow

	case theme.ColorNameScrollBar:
		return p.ScrollBar
	case theme.ColorNameScrollBarBackground:
		// Прозрачный: дорожка скроллбара в современных интерфейсах не
		// рисуется — виден только ползунок.
		return color.NRGBA{}
	case theme.ColorNameInnerWindowBorder:
		return p.Border
	case theme.ColorNameInnerWindowBorderInactive:
		return p.Border
	}
	return t.base.Color(name, v)
}

// Size переопределяет размеры, отвечающие за «характер» интерфейса.
//
// Радиусы: Fyne берёт их отсюда для кнопок, полей, карточек, диалогов и
// поповеров. Один набор значений на всё приложение — то, ради чего вообще
// стоит переопределять Size.
//
// Размеры текста НЕ меняются: кегли выводятся из SizeNameText, и трогать их
// значит разойтись с системными настройками доступности. Иерархия текста
// задаётся в typography.go относительными множителями.
func (t *Theme) Size(name fyne.ThemeSizeName) float32 {
	switch name {
	case theme.SizeNameButtonRadius:
		return 10
	case theme.SizeNameInputRadius:
		return 10
	case theme.SizeNameSelectionRadius:
		return 12
	case theme.SizeNameCardRadius:
		return 16
	case theme.SizeNameDialogRadius:
		return 16
	case theme.SizeNamePopupRadius:
		return 14
	case theme.SizeNameMenuRadius:
		return 12
	case theme.SizeNameInnerWindowRadius:
		return 16

	case theme.SizeNameInputBorder:
		return 1
	case theme.SizeNameSeparatorThickness:
		// Тонкая линия: разделитель должен намекать, а не делить экран.
		return 1
	case theme.SizeNameSplitThickness:
		return 1

	case theme.SizeNameScrollBar:
		return 10
	case theme.SizeNameScrollBarSmall:
		return 6
	case theme.SizeNameScrollBarRadius:
		return 5

	case theme.SizeNamePadding:
		// Базовая плотность промежутков в стандартных контейнерах Fyne
		// (VBox/HBox). Было 8 — на десктопе это читается как планшет:
		// каждый VBox добавлял по 8 unit, а страницы вдобавок вставляли
		// явные распоры, давая двойной зазор. 5 — десктопная плотность.
		return 5
	case theme.SizeNameInnerPadding:
		return 5
	case theme.SizeNameLineSpacing:
		return 2
	}
	return t.base.Size(name)
}

// Font делегирует дефолтной теме: подмена шрифта ломает метрики на чужих ОС.
func (t *Theme) Font(s fyne.TextStyle) fyne.Resource { return t.base.Font(s) }

// Icon делегирует дефолтной теме: у неё полный набор иконок контролов.
func (t *Theme) Icon(n fyne.ThemeIconName) fyne.Resource { return t.base.Icon(n) }
