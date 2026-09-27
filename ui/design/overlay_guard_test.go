package design

import (
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"
)

// TestTrailingInteractiveGuard — прозрачный overlay нельзя класть поверх
// интерактивных контролов.
//
// Регрессионный тест на дефект «некоторые кнопки не нажимаются»: holder,
// добавленный верхним слоем в Stack, перехватывает попадания, и вложенный
// Button/Entry/Select перестаёт получать события. CardRow поэтому добавляет
// holder только при неинтерактивном trailing.
func TestTrailingInteractiveGuard(t *testing.T) {
	if trailingInteractive(nil) {
		t.Fatal("nil trailing reported as interactive; plain rows would lose their hover")
	}

	// Простые декоративные объекты — можно накрывать.
	rect := canvas.NewRectangle(nil)
	if trailingInteractive(rect) {
		t.Fatal("a plain rectangle reported as interactive; rows with icons would stop being clickable")
	}
	if trailingInteractive(canvas.NewText("42 ms", nil)) {
		t.Fatal("plain text reported as interactive")
	}

	// Кнопка — нельзя: её клик был бы съеден overlay'ем.
	btn := widget.NewButton("Open", func() {})
	if !trailingInteractive(btn) {
		t.Fatal("a Button reported as non-interactive; an overlay would swallow its clicks")
	}

	// Контейнер с кнопкой внутри тоже нельзя: снаружи он не Tappable, но
	// содержит реальный контрол.
	box := container.NewHBox(widget.NewButton("Open", func() {}))
	if !trailingInteractive(box) {
		t.Fatal("a container holding a Button reported as non-interactive; nested controls would be unreachable")
	}
}

// TestClickableCardHasNoOverlayLayer — у ClickableCard не должно быть
// прозрачного слоя поверх содержимого.
func TestClickableCardHasNoOverlayLayer(t *testing.T) {
	c := NewClickableCard("Title", "Summary", func() {})
	r := c.CreateRenderer()
	objs := r.Objects()
	// Слои: фон и граница (canvas.Rectangle) + содержимое.
	for _, o := range objs {
		if _, ok := o.(*canvas.Rectangle); ok {
			continue
		}
		// Содержимое — единственный не-rectangle объект.
		if _, ok := o.(*fyne.Container); !ok {
			t.Fatalf("unexpected layer %T in ClickableCard renderer", o)
		}
	}
}
