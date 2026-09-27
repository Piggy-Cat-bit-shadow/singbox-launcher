// File tint.go — перекраска themed-иконок (SPEC 145).
//
// **Зачем.** Наши навигационные иконки — SVG с `currentColor`, обёрнутые в
// `theme.NewThemedResource`. Fyne передаёт им ровно один цвет темы
// (ColorNameForeground), поэтому одна и та же иконка не может быть
// приглушённой в обычном состоянии и акцентной в выбранном. Проблема решается
// на уровне ресурса: мы подменяем SVG перед отдачей в `widget.Icon`.
//
// **Почему не «нарисовать две иконки».** Два отдельных SVG на каждый пункт
// (обычный и акцентный) удвоили бы набор и разошлись бы при правке геометрии.
// Подстановка цвета оставляет один источник геометрии.
//
// **Что именно подменяется.** В `currentColor` подставляется hex нужного
// цвета из палитры. Тема Fyne оборачивает themed-ресурс и переписывает
// `currentColor` своим цветом при отрисовке — поэтому здесь мы снимаем
// themed-обёртку и отдаём обычный статический ресурс с уже подставленным
// цветом. Так выбор цвета остаётся за палитрой приложения, а не за темой.
//
// go1.20-совместимо (Win7-джоба): без slices/maps/min/max/clear.
package design

import (
	"fmt"
	"image/color"
	"strings"

	"fyne.io/fyne/v2"
)

// tintResource — ресурс с подставленным цветом вместо `currentColor`.
type tintResource struct {
	res  fyne.Resource
	tint color.Color

	// cached — результат подстановки. Пересчитывается только при смене
	// цвета: Refresh() у иконки вызывается часто (наведение, выбор), а
	// строковая замена на каждый вызов — лишняя работа в UI-потоке.
	cached     *fyne.StaticResource
	cachedFor  string
	cachedName string
}

var _ fyne.Resource = (*tintResource)(nil)

// Name возвращает имя ресурса. Fyne использует его как ключ кэша
// отрисованных изображений, поэтому цвет обязан входить в имя: без этого
// перекрашенная иконка взяла бы из кэша старую картинку.
func (t *tintResource) Name() string {
	return fmt.Sprintf("%s#%s", t.res.Name(), tintHex(t.tint))
}

// Content возвращает SVG с подставленным цветом.
func (t *tintResource) Content() []byte {
	hex := tintHex(t.tint)
	if t.cached != nil && t.cachedFor == hex && t.cachedName == t.res.Name() {
		return t.cached.Content()
	}
	body := strings.ReplaceAll(string(t.res.Content()), "currentColor", hex)
	t.cached = &fyne.StaticResource{
		StaticName:    t.res.Name() + "#" + hex,
		StaticContent: []byte(body),
	}
	t.cachedFor = hex
	t.cachedName = t.res.Name()
	return t.cached.Content()
}

// tintHex — цвет в виде #rrggbb.
//
// RGBA() возвращает 16-битные компоненты; берём старший байт каждого.
func tintHex(c color.Color) string {
	if c == nil {
		return "#000000"
	}
	r, g, b, _ := c.RGBA()
	return fmt.Sprintf("#%02x%02x%02x", uint8(r>>8), uint8(g>>8), uint8(b>>8))
}
