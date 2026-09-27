package ui

import (
	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"

	"singbox-launcher/core"
	"singbox-launcher/core/services"
	"singbox-launcher/ui/design"
)

// Вкладки Local и Remote (SPEC 098 §2.1).
//
// Обе устроены одинаково: СЛЕВА список прокси, СПРАВА управление. Разница
// только в том, чем управляем — своим ядром или списком чужих машин.
// Одинаковая геометрия не эстетика: список прокси на обеих вкладках это
// буквально один виджет с одним поведением, и переход между вкладками не
// требует переучиваться.
//
// До SPEC 098 это были вкладки Core и Servers, а управление удалёнными
// машинами жило в трёх местах сразу (шапка Servers, окно подключения, визард).
// Чтобы настроить одну машину, надо было обойти три экрана, и ни на одном не
// было видно её целиком.

// Ширины колонок — из дизайн-токенов (SPEC 144).
//
// Раньше здесь стояли 395 и 165 «по факту». Правая колонка в 165 logical
// units не вмещает современный dashboard: версию ядра, адрес машины и
// строки действий. Теперь колонки заданы долей (устойчива к ресайзу) плюс
// минимальными ширинами, ниже которых панель не схлопывается.
//
// Доля сама по себе тут не работала бы: HSplit считает offset от MinSize
// дочерних элементов, а список прокси просит много ширины и продавливает
// разделитель вправо. Поэтому правой задаётся собственный минимум
// (minSizeBox), а offset вычисляется из фактических ширин.
const (
	leftColumnWidth  = design.MinPaneWidthList
	rightColumnWidth = design.MinPaneWidthPanel
)

// splitColumnRatio — доля окна под левую колонку при стартовом размере.
const splitColumnRatio = design.SplitRatioPrimary

// startWithMinimalLeft ставит разделитель так, чтобы при старте левая колонка
// была ровно leftColumnWidth, а всё лишнее место доставалось правой.
//
// Фиксированная доля этого не даёт: на окне шире минимума 0.705 отдают левой
// лишние пиксели, хотя список прокси в них не нуждается — растягивать надо
// панель управления. Реальную ширину сплита узнаём только после раскладки,
// поэтому доля пересчитывается в minSplitLayout при каждом Resize.
type minSplitLayout struct {
	split *container.Split
	// applied — стартовая доля уже выставлена по реальной ширине. Дальше не
	// вмешиваемся: иначе каждый ресайз окна отменял бы разделитель, который
	// пользователь передвинул руками.
	applied bool
}

func (l *minSplitLayout) Layout(objects []fyne.CanvasObject, size fyne.Size) {
	for _, o := range objects {
		o.Resize(size)
		o.Move(fyne.NewPos(0, 0))
	}
	if l.applied || size.Width <= 0 {
		return
	}
	l.applied = true
	offset := float64(leftColumnWidth) / float64(size.Width)
	if offset > splitColumnRatio {
		// Окно уже стартового: доля больше базовой означала бы, что правая
		// колонка ужимается сильнее своего минимума.
		offset = splitColumnRatio
	}
	if l.split.Offset != offset {
		l.split.SetOffset(offset)
	}
}

func (l *minSplitLayout) MinSize(objects []fyne.CanvasObject) fyne.Size {
	var out fyne.Size
	for _, o := range objects {
		m := o.MinSize()
		if m.Width > out.Width {
			out.Width = m.Width
		}
		if m.Height > out.Height {
			out.Height = m.Height
		}
	}
	return out
}

// withMinimalLeftColumn оборачивает сплит так, чтобы стартовый offset считался
// от фактической ширины, а не от предполагаемой.
func withMinimalLeftColumn(split *container.Split) fyne.CanvasObject {
	split.SetOffset(splitColumnRatio)
	return container.New(&minSplitLayout{split: split}, split)
}

// CreateLocalTab — вкладка Local: слева прокси локального ядра, справа
// управление этим ядром (бывшая вкладка Core целиком).
//
// Возвращает и панель списка: у Local и Remote независимые состояния, и
// переключение вкладки обязано отдать разделяемые слоты UIService той панели,
// которую пользователь видит (ProxyListPanel.Activate).
//
// notice — плашка страховки «ядро отвергло узел» (SPEC 132 §6.1) НАД списком
// узлов. nil допустим: вкладка строится и без неё (тесты, headless).
// Только на Local: страховка выключает узлы ЛОКАЛЬНОГО состояния, и на Remote
// эта полоса говорила бы о чужой машине.
func CreateLocalTab(ac *core.AppController, notice fyne.CanvasObject) (fyne.CanvasObject, *ProxyListPanel) {
	panel := CreateProxyListPanel(ac, services.ScopeLocal)
	left := panel.Content
	if notice != nil {
		// Border с плашкой сверху: скрытый контейнер MinSize не занимает, и
		// пока выключений нет, список стоит ровно там же, где стоял.
		left = container.NewBorder(notice, nil, nil, nil, panel.Content)
	}
	split := container.NewHSplit(
		left,
		withColumnWidth(CreateCoreDashboardTab(ac), rightColumnWidth),
	)
	return withMinimalLeftColumn(split), panel
}

// withColumnWidth фиксирует минимальную ширину колонки, не трогая высоту:
// без этого HSplit отдаёт всё место тому, кто просит больше, и правая
// колонка исчезает.
func withColumnWidth(content fyne.CanvasObject, width float32) fyne.CanvasObject {
	return container.New(&minSizeBox{min: fyne.NewSize(width, 0)}, content)
}

// CreateRemoteTab — вкладка Remote: слева прокси ВЫБРАННОЙ машины, справа
// список машин с управлением.
//
// onSelectionChanged перезагружает левую колонку после смены активной машины.
// Без этого список остался бы с узлами предыдущей — то есть показывал бы
// чужие данные под именем новой машины (нарушение инварианта §5.3).
func CreateRemoteTab(ac *core.AppController) (fyne.CanvasObject, *ProxyListPanel) {
	proxyPanel := CreateProxyListPanel(ac, services.ScopeRemote)
	// Обновляем СВОЮ панель напрямую, а не через UIService.RefreshAPIFunc:
	// выбор машины касается списка Remote, и промахнуться мимо него в момент,
	// когда слоты принадлежат другой вкладке, нельзя.
	machines := CreateMachineListPanel(ac, proxyPanel)
	split := container.NewHSplit(proxyPanel.Content, withColumnWidth(machines, rightColumnWidth))
	return withMinimalLeftColumn(split), proxyPanel
}

// MinWindowSize — нижняя граница размера главного окна (SPEC 144).
//
// Выведена из содержимого, а не назначена: сайдбар (208) плюс обе колонки
// контента (420 + 300) плюс поля. Прежнее значение 395+165 = 560 не
// помещало даже собственную правую панель, хотя комментарий рядом обещал
// 1000 — окно сжималось до нечитаемого состояния.
//
// Хранится здесь как обёртка над дизайн-токеном: часть кода и тестов
// ссылается на это имя.
var MinWindowSize = design.MinWindowSize()

// DefaultWindowSize — стартовый размер главного окна (SPEC 144).
var DefaultWindowSize = design.WindowSize

// minSizeBox — контейнер, навязывающий содержимому нижнюю границу размера.
//
// Fyne не даёт окну стать меньше MinSize его контента, поэтому нижняя граница
// окна задаётся именно так, а не через SetFixedSize (тот запретил бы и
// растягивание, а §3.2 требует «растянуть можно, сжать нельзя»).
type minSizeBox struct {
	min fyne.Size
}

func (b *minSizeBox) Layout(objects []fyne.CanvasObject, size fyne.Size) {
	for _, o := range objects {
		o.Resize(size)
		o.Move(fyne.NewPos(0, 0))
	}
}

func (b *minSizeBox) MinSize(objects []fyne.CanvasObject) fyne.Size {
	// Максимум из объявленного минимума и настоящего минимума контента:
	// занизить второй значило бы разрешить обрезание виджетов, которые сами
	// просят больше.
	out := b.min
	for _, o := range objects {
		m := o.MinSize()
		if m.Width > out.Width {
			out.Width = m.Width
		}
		if m.Height > out.Height {
			out.Height = m.Height
		}
	}
	return out
}

// WithMinWindowSize оборачивает контент главного окна, задавая ему нижнюю
// границу MinWindowSize.
func WithMinWindowSize(content fyne.CanvasObject) fyne.CanvasObject {
	return container.New(&minSizeBox{min: MinWindowSize}, content)
}
