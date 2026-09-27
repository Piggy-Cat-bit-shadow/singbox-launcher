// File palette.go — семантические цвета приложения (SPEC 145).
//
// **Почему явные hex, а не смешение цветов Fyne.** В SPEC 144 палитра
// выводилась из цветов дефолтной темы (`blend(foreground, background, k)`).
// Это давало серо-нейтральный результат и — главное — сохраняло визуальный
// характер Fyne: какой бы layout ни был, приложение оставалось узнаваемо
// «финевым». Здесь палитра задана явными значениями в стиле macOS/Clash:
// это и есть содержательная часть переработки, а не косметика.
//
// **Две палитры, один набор ролей.** Светлая и тёмная описывают одни и те же
// роли (Background, Surface, Border, TextPrimary…), поэтому компонент никогда
// не спрашивает «какая тема» — он берёт роль. Переключение системной темы
// меняет только то, какая структура вернулась.
//
// Ни одно значение здесь не является «чистым» чёрным или белым: #000 как фон
// и #FFF как поверхность дают максимальный контраст и на десктопе выглядят
// резко. Диапазон сознательно узкий — слои различаются на несколько единиц.
//
// go1.20-совместимо (Win7-джоба): без slices/maps/min/max/clear.
package design

import (
	"image/color"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
)

// Palette — набор семантических ролей. Поля, а не методы: палитру удобно
// перечислять и сравнивать, а поведение живёт в Theme.
type Palette struct {
	// Background — фон окна под всеми поверхностями.
	Background color.NRGBA
	// Sidebar — фон навигационной колонки. Обязан отличаться от Background:
	// на этом различии держится отделение навигации от контента.
	Sidebar color.NRGBA
	// Surface — поверхность карточек: самый светлый слой в светлой теме,
	// самый «приподнятый» в тёмной.
	Surface color.NRGBA
	// SurfaceAlt — второстепенная поверхность: поля ввода, вложенные блоки.
	SurfaceAlt color.NRGBA
	// SurfaceHover — фон под курсором.
	SurfaceHover color.NRGBA
	// SurfaceSelected — фон выбранного пункта. Мягкая заливка, НЕ насыщенный
	// акцент: выбранная строка не должна «гореть».
	SurfaceSelected color.NRGBA

	// Border — обычная граница: едва заметная.
	Border color.NRGBA
	// BorderStrong — граница полей ввода и акцентных элементов.
	BorderStrong color.NRGBA

	// TextPrimary — основной текст.
	TextPrimary color.NRGBA
	// TextSecondary — описания, подписи под заголовками.
	TextSecondary color.NRGBA
	// TextMuted — самый тихий текст: единицы измерения, метаданные.
	TextMuted color.NRGBA

	// Primary — акцент: выбранный пункт, главная кнопка.
	Primary color.NRGBA
	// PrimaryHover — акцент под курсором (чуть темнее в светлой теме,
	// чуть светлее в тёмной).
	PrimaryHover color.NRGBA
	// OnPrimary — текст на акцентной заливке.
	OnPrimary color.NRGBA

	// Success / Warning / Danger — статусы.
	Success color.NRGBA
	Warning color.NRGBA
	Danger  color.NRGBA

	// Focus — цвет фокуса клавиатуры.
	Focus color.NRGBA
	// ScrollBar — ползунок прокрутки.
	ScrollBar color.NRGBA
	// Shadow — подложка под карточками. Почти прозрачная: используется как
	// смещение на 1–2 unit, а не как blur.
	Shadow color.NRGBA

	// RowActiveBackground — фон строки узла, который сейчас используется
	// ядром. Семантическая роль, а не «светло-зелёный»: в тёмной теме фон
	// обязан быть тёмным, иначе строка выглядит подсвеченной маркером.
	RowActiveBackground color.NRGBA
	// RowSelectedBackground — фон строки, отмеченной пользователем
	// (множественный выбор перед групповой операцией).
	RowSelectedBackground color.NRGBA
}

// lightPalette — светлая схема.
//
// Background намеренно НЕ белый: если фон окна белый, а карточка тоже белая,
// карточка перестаёт читаться и всё «висит в воздухе». Здесь фон чуть
// темнее поверхности — именно это создаёт слоистость без теней.
var lightPalette = Palette{
	// Фон чуть темнее, чем в первой редакции: на светлой поверхности карточки
	// «белое на почти белом» читалось как отсутствие слоёв.
	Background: color.NRGBA{R: 0xF4, G: 0xF4, B: 0xF7, A: 0xFF},
	Sidebar:    color.NRGBA{R: 0xEC, G: 0xEC, B: 0xF0, A: 0xFF},
	Surface:    color.NRGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF},

	SurfaceAlt: color.NRGBA{R: 0xF2, G: 0xF2, B: 0xF5, A: 0xFF},
	// Hover ощутимее: раньше он был практически невидим.
	SurfaceHover: color.NRGBA{R: 0xE3, G: 0xE3, B: 0xE8, A: 0xFF},
	// Выбранный пункт: заметная, но не «синяя заливка» — accent tint ~12 %.
	SurfaceSelected: color.NRGBA{R: 0xDC, G: 0xE8, B: 0xFB, A: 0xFF},

	// Границы видны: раньше карточка почти не отделялась от фона.
	Border:       color.NRGBA{R: 0xDC, G: 0xDC, B: 0xE1, A: 0xFF},
	BorderStrong: color.NRGBA{R: 0xC8, G: 0xC8, B: 0xCF, A: 0xFF},

	TextPrimary:   color.NRGBA{R: 0x1C, G: 0x1C, B: 0x1E, A: 0xFF},
	TextSecondary: color.NRGBA{R: 0x5A, G: 0x5A, B: 0x60, A: 0xFF},
	// Muted только для действительно второстепенного: 0x98 был почти нечитаем
	// на белом и делал подписи карточек похожими на disabled.
	TextMuted: color.NRGBA{R: 0x7C, G: 0x7C, B: 0x82, A: 0xFF},

	Primary:      color.NRGBA{R: 0x00, G: 0x7A, B: 0xFF, A: 0xFF},
	PrimaryHover: color.NRGBA{R: 0x00, G: 0x6C, B: 0xE0, A: 0xFF},
	OnPrimary:    color.NRGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF},

	Success: color.NRGBA{R: 0x28, G: 0xA7, B: 0x45, A: 0xFF},
	Warning: color.NRGBA{R: 0xE8, G: 0x8B, B: 0x00, A: 0xFF},
	Danger:  color.NRGBA{R: 0xE5, G: 0x3E, B: 0x3E, A: 0xFF},

	Focus:     color.NRGBA{R: 0x00, G: 0x7A, B: 0xFF, A: 0x66},
	ScrollBar: color.NRGBA{R: 0xC5, G: 0xC5, B: 0xCA, A: 0xFF},
	Shadow:    color.NRGBA{R: 0x00, G: 0x00, B: 0x00, A: 0x0F},

	// Спокойные оттенки вместо прежних насыщенных (144/238/144 и 135/206/250):
	// строка — фон для текста, а не акцент.
	RowActiveBackground:   color.NRGBA{R: 0xDF, G: 0xF3, B: 0xE1, A: 0xFF},
	RowSelectedBackground: color.NRGBA{R: 0xDF, G: 0xEA, B: 0xFB, A: 0xFF},
}

// darkPalette — тёмная схема.
//
// Фон не чёрный (#151517), поверхность чуть светлее. В тёмной теме разница
// между слоями воспринимается слабее, поэтому шаг между Background и Surface
// больше, чем в светлой — иначе карточки сливаются.
var darkPalette = Palette{
	Background: color.NRGBA{R: 0x15, G: 0x15, B: 0x17, A: 0xFF},
	Sidebar:    color.NRGBA{R: 0x1C, G: 0x1C, B: 0x1E, A: 0xFF},
	Surface:    color.NRGBA{R: 0x24, G: 0x24, B: 0x26, A: 0xFF},

	SurfaceAlt:      color.NRGBA{R: 0x2C, G: 0x2C, B: 0x2E, A: 0xFF},
	SurfaceHover:    color.NRGBA{R: 0x32, G: 0x32, B: 0x35, A: 0xFF},
	SurfaceSelected: color.NRGBA{R: 0x2B, G: 0x3A, B: 0x52, A: 0xFF},

	Border:       color.NRGBA{R: 0x38, G: 0x38, B: 0x3A, A: 0xFF},
	BorderStrong: color.NRGBA{R: 0x48, G: 0x48, B: 0x4C, A: 0xFF},

	TextPrimary:   color.NRGBA{R: 0xF5, G: 0xF5, B: 0xF7, A: 0xFF},
	TextSecondary: color.NRGBA{R: 0xAE, G: 0xAE, B: 0xB2, A: 0xFF},
	TextMuted:     color.NRGBA{R: 0x8E, G: 0x8E, B: 0x93, A: 0xFF},

	// В тёмной теме акцент светлее: #007AFF на тёмном фоне читается хуже.
	Primary:      color.NRGBA{R: 0x0A, G: 0x84, B: 0xFF, A: 0xFF},
	PrimaryHover: color.NRGBA{R: 0x2E, G: 0x96, B: 0xFF, A: 0xFF},
	OnPrimary:    color.NRGBA{R: 0xFF, G: 0xFF, B: 0xFF, A: 0xFF},

	Success: color.NRGBA{R: 0x32, G: 0xD7, B: 0x4B, A: 0xFF},
	Warning: color.NRGBA{R: 0xFF, G: 0x9F, B: 0x0A, A: 0xFF},
	Danger:  color.NRGBA{R: 0xFF, G: 0x45, B: 0x3A, A: 0xFF},

	Focus:     color.NRGBA{R: 0x0A, G: 0x84, B: 0xFF, A: 0x80},
	ScrollBar: color.NRGBA{R: 0x48, G: 0x48, B: 0x4C, A: 0xFF},
	Shadow:    color.NRGBA{R: 0x00, G: 0x00, B: 0x00, A: 0x33},

	// В тёмной теме те же роли темнее фона карточки, а не светлее.
	RowActiveBackground:   color.NRGBA{R: 0x1E, G: 0x33, B: 0x25, A: 0xFF},
	RowSelectedBackground: color.NRGBA{R: 0x1F, G: 0x2C, B: 0x42, A: 0xFF},
}

// PaletteFor возвращает палитру для варианта темы.
//
// Сравнение — ТОЛЬКО с официальными константами fyne/theme (VariantDark == 0,
// VariantLight == 1). Никаких собственных числовых значений: именно рукописный
// алиас сделал так, что светлая системная тема получала тёмную палитру.
func PaletteFor(v fyne.ThemeVariant) Palette {
	if v == theme.VariantDark {
		return darkPalette
	}
	return lightPalette
}
