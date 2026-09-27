// Package design — единый источник размеров, типографики и цветов для UI
// лаунчера (SPEC 144).
//
// **Зачем отдельный пакет.** До SPEC 144 отступы, радиусы и ширины жили
// магическими числами прямо в местах использования: `container.NewPadded`
// поверх `NewPadded`, `395/165` как «ширины колонок», `8/10/12/16/20`
// вперемешку. Правка одного экрана не переносилась на соседний, и интерфейс
// расходился по ритму. Здесь — единственное место, где эти числа объявлены.
//
// **Только константы и чистые хелперы.** Пакет не знает ни о контроллере, ни
// о сервисах, ни о состоянии приложения: его можно импортировать откуда
// угодно без риска циклической зависимости.
//
// **Logical units.** Все размеры — в координатах Fyne, не в физических
// пикселях. Fyne сам умножает их на масштаб canvas. Поэтому здесь запрещены
// (и отсутствуют) `canvas.Scale()`, проверки Retina и `runtime.GOOS`: любое
// такое умножение дало бы двойное масштабирование на Retina и расхождение
// на Windows при 125 %.
//
// go1.20-совместимо (Win7-джоба): без slices/maps/min/max/clear.
package design

import "fyne.io/fyne/v2"

// Шкала отступов — четвёртый ритм.
//
// Ступеней девять, а не шесть: современная десктопная вёрстка отличается от
// плотной админки именно количеством «воздуха», и для внешних полей страницы
// нужны значения за пределами 24. Короткий список не даёт повода изобретать
// «13» или «19» на месте: если значение не подходит — неверно выбран уровень.
const (
	SpaceXS  float32 = 4
	SpaceS   float32 = 8
	SpaceM   float32 = 12
	SpaceL   float32 = 16
	SpaceXL  float32 = 20
	Space2XL float32 = 24
	Space3XL float32 = 32
	Space4XL float32 = 40
	Space5XL float32 = 48
)

// Международные блоки: расстояние между смысловыми секциями страницы.
const (
	// SectionGap — между секциями внутри одной страницы.
	SectionGap float32 = Space2XL
	// BlockGap — между связанными блоками одной секции.
	BlockGap float32 = SpaceL
)

// Радиусы. Значения различаются осознанно: чем крупнее поверхность, тем
// больше радиус, но потолок невысок — иначе интерфейс читается как
// мобильное приложение, растянутое на десктоп.
const (
	// RadiusControl — мелкие элементы: бейджи-плашки, чипы.
	RadiusControl float32 = 8
	// RadiusButton — кнопки и поля ввода.
	RadiusButton float32 = 10
	// RadiusNav — подсветка выбранного пункта навигации. Заметно больше
	// кнопки: это «pill», а не строка таблицы.
	RadiusNav float32 = 12
	// RadiusCard — карточки и секции — основной контейнер контента.
	RadiusCard float32 = 16
	// RadiusHero — крупная карточка Home: чуть мягче обычной.
	RadiusHero float32 = 18
	// RadiusDialog — модальные окна.
	RadiusDialog float32 = 16
)

// Геометрия сайдбара.
//
// Ширина выбрана так, чтобы в окне 1180 (см. DefaultWindowSize) сайдбар
// занимал ~17.6 % — привычная доля навигационной колонки в десктопных
// приложениях. Больше 216 начинает отъедать место у контента, меньше 204 —
// обрезает «Diagnostics» и длинные русские подписи.
const (
	// SidebarWidth — полная ширина навигационной колонки.
	SidebarWidth float32 = 224
	// SidebarPadding — внутренние отступы сайдбара по горизонтали.
	SidebarPadding float32 = 14
	// SidebarIdentityHeight — высота блока с именем приложения.
	SidebarIdentityHeight float32 = 68
	// SidebarTopGap — отступ от верха окна до первого пункта.
	SidebarTopGap float32 = SpaceS
	// NavItemHeight — высота пункта навигации.
	NavItemHeight float32 = 46
	// NavIconSize — размер иконки пункта.
	NavIconSize float32 = 20
	// NavIconGap — зазор между иконкой и текстом.
	NavIconGap float32 = SpaceM
	// NavItemGap — вертикальный зазор между пунктами.
	NavItemGap float32 = 2
	// NavSectionGap — зазор перед заголовком группы.
	NavSectionGap float32 = SpaceL
	// NavSectionLabelHeight — высота заголовка группы.
	NavSectionLabelHeight float32 = 24
)

// Геометрия контентной области.
const (
	// ContentPaddingH — горизонтальные поля страницы.
	ContentPaddingH float32 = Space4XL
	// ContentPaddingV — вертикальные поля страницы.
	ContentPaddingV float32 = Space3XL
	// PageHeaderHeight — высота шапки страницы.
	PageHeaderHeight float32 = 72
	// CardPadding — внутренние отступы карточки.
	CardPadding float32 = SpaceXL
	// CardGap — зазор между карточками в колонке.
	CardGap float32 = SpaceL
	// RowHeight — минимальная высота строки настроек.
	RowHeight float32 = 48
	// RowMinHeight — высота строки карточки с двумя строками текста.
	RowMinHeight float32 = 52
	// ToolbarHeight — высота панели инструментов.
	ToolbarHeight float32 = 44
	// MaxContentWidth — предел ширины контентной колонки. Карточка шириной
	// 1400 выглядит сломанной; на широком окне контент центрируется.
	MaxContentWidth float32 = 860
	// SegmentedHeight — высота сегментированного переключателя.
	SegmentedHeight float32 = 34
)

// Размеры окна.
//
// Выведены из содержимого, а не назначены «на глаз». До SPEC 144 здесь стоял
// `leftColumnWidth + rightColumnWidth` = 395 + 165 = 560 при комментарии,
// обещавшем 1000: окно сжималось до состояния, когда правая колонка
// схлопывалась. Теперь минимум считается от сайдбара плюс две читаемые
// колонки контента.
const (
	// DefaultWindowWidth / Height — стартовый размер главного окна.
	DefaultWindowWidth  float32 = 1220
	DefaultWindowHeight float32 = 800

	// MinWindowWidth / Height — нижняя граница. Ниже сайдбар + две колонки
	// перестают помещаться, и вместо деградации получается каша из
	// обрезанных подписей. 960 проверено на самой длинной локализации
	// (русской) и на узких подписях кнопок.
	MinWindowWidth  float32 = 1000
	MinWindowHeight float32 = 680
)

// Доли колонок на страницах Local и Remote.
//
// Раньше колонки задавались абсолютными ширинами (395 и 165), и правая
// получалась слишком узкой для современного dashboard: версия ядра, адрес
// машины и строки действий туда не влезали. Доля устойчива к ресайзу, а
// минимальные ширины (ниже) не дают панели схлопнуться в ноль.
const (
	// SplitRatioPrimary — доля левой (список) колонки при первой раскладке.
	SplitRatioPrimary float64 = 0.62
	// MinPaneWidthList — минимум для списка узлов/машин: уже — имена
	// начинают резаться многоточием, а колонка задержки уезжает.
	MinPaneWidthList float32 = 420
	// MinPaneWidthPanel — минимум для панели управления справа.
	MinPaneWidthPanel float32 = 300
)

// WindowSize возвращает стартовый размер главного окна.
func WindowSize() fyne.Size {
	return fyne.NewSize(DefaultWindowWidth, DefaultWindowHeight)
}

// MinWindowSize возвращает нижнюю границу размера главного окна.
func MinWindowSize() fyne.Size {
	return fyne.NewSize(MinWindowWidth, MinWindowHeight)
}
