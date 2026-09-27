package design

import (
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/theme"
)

// TestPaletteForOfficialVariants — палитра выбирается по ОФИЦИАЛЬНЫМ константам
// Fyne.
//
// Регрессионный тест на конкретный дефект: в SPEC 145 были заведены
// собственные числовые алиасы variant'а (Dark = 1, Light = 2), тогда как в
// Fyne VariantDark == 0 и VariantLight == 1. Из-за этого светлая системная
// тема (Light == 1) попадала в тёмную палитру, и окно рисовалось тёмным под
// светлой нативной шапкой.
//
// Проверяются только ключевые роли: полный перебор hex-значений сделал бы
// тест хрупким перед любым изменением оттенка, а ловит он именно выбор
// палитры.
func TestPaletteForOfficialVariants(t *testing.T) {
	// Значения констант — часть контракта Fyne. Если они когда-нибудь
	// изменятся, тест должен упасть здесь, а не в интерфейсе.
	if theme.VariantDark != 0 {
		t.Fatalf("theme.VariantDark = %d, want 0", theme.VariantDark)
	}
	if theme.VariantLight != 1 {
		t.Fatalf("theme.VariantLight = %d, want 1", theme.VariantLight)
	}

	light := PaletteFor(theme.VariantLight)
	dark := PaletteFor(theme.VariantDark)

	// Background: фон окна — самое заметное различие двух схем.
	if light.Background != lightPalette.Background {
		t.Fatalf("VariantLight gave Background %v, want the light palette %v",
			light.Background, lightPalette.Background)
	}
	if dark.Background != darkPalette.Background {
		t.Fatalf("VariantDark gave Background %v, want the dark palette %v",
			dark.Background, darkPalette.Background)
	}

	// Sidebar: колонка навигации обязана отличаться от фона в обеих схемах.
	if light.Sidebar != lightPalette.Sidebar {
		t.Fatalf("VariantLight gave Sidebar %v, want %v", light.Sidebar, lightPalette.Sidebar)
	}
	if dark.Sidebar != darkPalette.Sidebar {
		t.Fatalf("VariantDark gave Sidebar %v, want %v", dark.Sidebar, darkPalette.Sidebar)
	}

	// Primary: акцент. В светлой схеме #007AFF, в тёмной он светлее.
	if light.Primary != lightPalette.Primary {
		t.Fatalf("VariantLight gave Primary %v, want %v", light.Primary, lightPalette.Primary)
	}
	if dark.Primary != darkPalette.Primary {
		t.Fatalf("VariantDark gave Primary %v, want %v", dark.Primary, darkPalette.Primary)
	}

	// Схемы обязаны реально различаться — иначе выбор варианта бессмыслен.
	if light.Background == dark.Background {
		t.Fatal("light and dark palettes have the same Background; variant is ignored")
	}
}

// TestThemeEffectiveVariant — режим внешнего вида приводит вариант к нужному.
//
// System обязан возвращать системный вариант БЕЗ изменений: именно это даёт
// совпадение содержимого окна с нативной шапкой.
func TestThemeEffectiveVariant(t *testing.T) {
	cases := []struct {
		mode      AppearanceMode
		requested fyne.ThemeVariant
		want      fyne.ThemeVariant
	}{
		{AppearanceSystem, theme.VariantLight, theme.VariantLight},
		{AppearanceSystem, theme.VariantDark, theme.VariantDark},
		{AppearanceLight, theme.VariantDark, theme.VariantLight},
		{AppearanceLight, theme.VariantLight, theme.VariantLight},
		{AppearanceDark, theme.VariantLight, theme.VariantDark},
		{AppearanceDark, theme.VariantDark, theme.VariantDark},
		// Пустое и неизвестное значение трактуются как System.
		{"", theme.VariantLight, theme.VariantLight},
		{"nonsense", theme.VariantDark, theme.VariantDark},
	}
	for _, c := range cases {
		th := NewTheme(c.mode)
		if got := th.effectiveVariant(c.requested); got != c.want {
			t.Fatalf("mode %q, requested %d: effectiveVariant = %d, want %d",
				c.mode, c.requested, got, c.want)
		}
	}
}

// TestThemeColorFollowsMode — Color() реально использует выбранный режим, а не
// только хранит его.
func TestThemeColorFollowsMode(t *testing.T) {
	// В светлом режиме при СИСТЕМНОЙ тёмной теме цвет обязан быть светлым:
	// это ровно тот случай, который был сломан.
	th := NewTheme(AppearanceLight)
	got := th.Color(theme.ColorNameBackground, theme.VariantDark)
	if got != lightPalette.Background {
		t.Fatalf("light mode returned %v for Background, want the light palette %v",
			got, lightPalette.Background)
	}

	th = NewTheme(AppearanceDark)
	got = th.Color(theme.ColorNameBackground, theme.VariantLight)
	if got != darkPalette.Background {
		t.Fatalf("dark mode returned %v for Background, want the dark palette %v",
			got, darkPalette.Background)
	}

	// System следует за системой.
	th = NewTheme(AppearanceSystem)
	if got := th.Color(theme.ColorNameBackground, theme.VariantLight); got != lightPalette.Background {
		t.Fatalf("system mode with a light system theme returned %v, want %v",
			got, lightPalette.Background)
	}
}
