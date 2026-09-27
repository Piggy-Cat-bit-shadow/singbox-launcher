package backup

import (
	"encoding/json"
	"testing"

	"singbox-launcher/core/state"
)

// Локальный снимок обязан пережить круг «экспорт 1.0 → разбор → импорт».
//
// У подписки nodes — кэш выдачи провайдера, и в файл он не едет: на приёмнике
// его наполнит первое же обновление. У снимка провайдера НЕТ, обновляться
// неоткуда, а URL пуст. Поэтому две вещи должны ехать вместе:
//
//   - сам состав (nodes), иначе источник приедет пустым и нечем его наполнить;
//   - признак input_kind, иначе снимок на приёмнике станет обычной подпиской
//     с пустым URL — то есть «обновляемой» в интерфейсе и заведомо нерабочей.
//
// Молчаливая потеря здесь — худший вид: файл выглядит восстановленным.
func TestExportCarriesLocalSnapshot(t *testing.T) {
	src := localSnapshotState()

	b, warns, err := Export10(src, ExportOptions{})
	if err != nil {
		t.Fatalf("Export10: %v", err)
	}
	for _, w := range warns {
		if w.Kind == string(state.SourceKindSubscription) {
			t.Errorf("снимок объявлен неподдержанным, хотя он едет: %v", w)
		}
	}

	rec := findSource10(t, b, "01LCL0000000000000000000")
	if rec.URL != "" {
		t.Errorf("url = %q, у снимка его быть не должно", rec.URL)
	}
	if rec.InputKind != state.SubscriptionInputLocalSnapshot {
		t.Errorf("input_kind = %q, ожидался local_snapshot", rec.InputKind)
	}
	if rec.LocalFilename != "nodes.json" {
		t.Errorf("local_filename = %q, ожидался nodes.json", rec.LocalFilename)
	}
	if len(rec.Nodes) != len(src.Sources[0].Nodes) {
		t.Fatalf("в файл поехало %d узлов из %d: снимок приедет пустым и его "+
			"нечем наполнить", len(rec.Nodes), len(src.Sources[0].Nodes))
	}
	if rec.Nodes[0].Tag != "n-1" || rec.Nodes[1].Tag != "n-2" {
		t.Errorf("состав снимка искажён: %+v", rec.Nodes)
	}
}

// Круг в пустое состояние: и состав, и признак на месте, и источник остаётся
// снимком — то есть после восстановления по-прежнему не ходит в сеть.
func TestLocalSnapshotSurvivesRestore(t *testing.T) {
	src := localSnapshotState()

	b, _, err := Export10(src, ExportOptions{})
	if err != nil {
		t.Fatalf("Export10: %v", err)
	}
	doc, err := json.Marshal(b)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	parsed, _, err := Parse(doc)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	dst := &state.State{}
	if _, err := ImportFile(dst, parsed, ImportOptions{}); err != nil {
		t.Fatalf("ImportFile: %v", err)
	}

	var got *state.Source
	for i := range dst.Sources {
		if dst.Sources[i].IsLocalSnapshot() {
			got = &dst.Sources[i]
			break
		}
	}
	if got == nil {
		t.Fatalf("после восстановления снимка нет: источники = %+v", dst.Sources)
	}
	if len(got.Nodes) != 2 {
		t.Errorf("узлов после восстановления %d, ожидалось 2", len(got.Nodes))
	}
	if got.LocalFilename != "nodes.json" {
		t.Errorf("local_filename = %q после круга", got.LocalFilename)
	}
	// Главное следствие: восстановленный снимок по-прежнему не обновляется.
	if state.CanRefreshSubscription(got) {
		t.Error("восстановленный снимок считается обновляемым: он уйдёт в сеть с пустым URL")
	}
}

// Обычная подписка не должна измениться: её кэш по-прежнему не едет, и
// input_kind остаётся пустым (читается как remote).
func TestExportStillDropsRemoteSubscriptionCache(t *testing.T) {
	st := &state.State{Sources: []state.Source{{
		ID:   "01REM0000000000000000000",
		Node: state.Node{Kind: state.SourceKindSubscription, Enabled: true},
		Name: "Provider",
		URL:  "https://example.com/sub",
		Nodes: []state.Node{
			{Kind: state.SourceKindServer, Tag: "cached-1", Enabled: true},
		},
	}}}

	b, _, err := Export10(st, ExportOptions{})
	if err != nil {
		t.Fatalf("Export10: %v", err)
	}
	rec := findSource10(t, b, "01REM0000000000000000000")
	if len(rec.Nodes) != 0 {
		t.Errorf("кэш подписки поехал в файл: %+v", rec.Nodes)
	}
	if rec.InputKind != "" {
		t.Errorf("input_kind = %q у обычной подписки, ожидалось пусто", rec.InputKind)
	}
	if rec.URL != "https://example.com/sub" {
		t.Errorf("url = %q, ожидался сохранённый адрес", rec.URL)
	}
}

func localSnapshotState() *state.State {
	return &state.State{Sources: []state.Source{{
		ID:            "01LCL0000000000000000000",
		Node:          state.Node{Kind: state.SourceKindSubscription, Enabled: true},
		Name:          "My Local Nodes",
		InputKind:     state.SubscriptionInputLocalSnapshot,
		LocalFilename: "nodes.json",
		Nodes: []state.Node{
			{Kind: state.SourceKindServer, Tag: "n-1", Enabled: true},
			{Kind: state.SourceKindServer, Tag: "n-2", Enabled: true},
		},
	}}}
}

func findSource10(t *testing.T, b *Backup10, id string) Source10 {
	t.Helper()
	for _, s := range b.Sources {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("источник %q не попал в файл: %+v", id, b.Sources)
	return Source10{}
}
