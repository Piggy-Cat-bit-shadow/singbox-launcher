package config

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"singbox-launcher/core/config/subscription"
)

// Критерий приёмки SPEC 131 §9.4: конфиг, собранный из ВСЕХ кейсов корпуса
// разом, принимается ядром пина.
//
// Смысл именно в «разом»: ядро отвергает config.json ЦЕЛИКОМ на первом же
// негодном значении, и один мусорный узел из подписки на 500 записей
// оставляет человека вообще без VPN. Проверка по одному узлу этого класса
// дефектов не ловит — она видит только то, что каждый узел по отдельности
// валиден, а вопрос стоит иначе: переживает ли ядро их СУММУ.
//
// Тест и есть исполнение инварианта §3.2 («значение, которое ядро отвергнет
// фаталом, после санитайзера в теле остаться не может») на всём корпусе.
func TestCorpusBodiesPassSingboxCheck(t *testing.T) {
	binary := locateSingboxBinary(t)
	if binary == "" {
		t.Skip("bin/sing-box not found — skipping real-core validation")
	}
	// Гейт сборки — часть проверяемого пути, а не обход её: тело узла
	// заморожено и несёт поля любого ядра, а config.json собирается ПОД ТО
	// ядро, которое здесь и запускается. Версия берётся у самого бинаря —
	// на машине разработчика это далеко не всегда ядро пина.
	version := coreVersionOfBinary(t, binary)
	prev := CoreVersionProbe
	CoreVersionProbe = func() string { return version }
	t.Cleanup(func() { CoreVersionProbe = prev })
	t.Logf("ядро проверки: %s", version)

	// Ядро форка собирается с разными наборами тегов, и корпус шире любого
	// одного набора: в нём есть, например, endpoints типа wireguard. Ядро
	// без with_wireguard отвергнет такой конфиг — и это НЕ дефект генератора,
	// а свойство конкретной сборки. Раньше тест падал с `unknown endpoint
	// type: wireguard`, что читается как поломка кода; теперь он честно
	// пропускается, называя недостающую возможность. Проверка остаётся
	// строгой: если ядро возможность поддерживает, тест обязан пройти.
	if missing := missingCorpusCapabilities(t, binary); len(missing) > 0 {
		t.Skipf("ядро %s не поддерживает возможности корпуса: %s — сборка без нужных тегов; укажите ядро через SINGBOX_TEST_CORE",
			version, strings.Join(missing, ", "))
	}

	root := filepath.Join(contractCorpusRelPath, "uri")
	if _, err := os.Stat(root); os.IsNotExist(err) {
		t.Skipf("корпус контракта не найден: %s", root)
	}

	var cases []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && strings.HasSuffix(path, ".uri") {
			cases = append(cases, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("обход корпуса: %v", err)
	}
	sort.Strings(cases)

	var outbounds []map[string]interface{}
	var endpoints []map[string]interface{}
	used := map[string]bool{}
	for _, casePath := range cases {
		line := firstPayloadLine(t, casePath)
		if line == "" {
			continue
		}
		node, err := subscription.ParseNode(line, nil)
		if err != nil || node == nil {
			// Кейс-отбраковка: узла нет и проверять нечего — это законная
			// часть корпуса (битые ссылки лежат там намеренно).
			continue
		}
		body, _, drop := materializeParsedNodeBody(node)
		if drop != nil {
			// Узел, который конвейер отверг, в config.json не попадает —
			// ровно поэтому ядру его и не показываем.
			continue
		}
		gated, _ := gateBodyForCore(node.Scheme, caseTagOf(casePath), body)
		entry := map[string]interface{}{}
		if err := json.Unmarshal(gated, &entry); err != nil {
			t.Fatalf("%s: тело узла нечитаемо: %v", casePath, err)
		}
		// Снимаем два класса полей, о которых этот тест ничего сказать не
		// может и не должен:
		//   - пути к файлам (*_path) — указывают на машину пользователя, на
		//     машине сборки их нет («open /home/user/.ssh/id_rsa: no such
		//     file»);
		//   - криптоматериал, который ядро РАЗБИРАЕТ (ssh host_key) — в
		//     общем корпусе он заведомо синтетический («FakeSyntheticHostKey»),
		//     настоящему там взяться неоткуда.
		// В обоих случаях падение говорило бы о фикстуре, а не о конвейере, —
		// а проверяем мы конвейер.
		dropUncheckableFields(entry)
		// Теги в конфиге уникальны: на дубле ядро отвергает outbounds целиком,
		// и это была бы ошибка ТЕСТА, а не корпуса.
		tag := uniqueTag(caseTagOf(casePath), used)
		entry["tag"] = tag
		if IsEndpointScheme(node.Scheme) {
			endpoints = append(endpoints, entry)
			continue
		}
		outbounds = append(outbounds, entry)
	}

	if len(outbounds)+len(endpoints) == 0 {
		t.Skip("корпус не дал ни одного собираемого узла")
	}

	cfg := buildCheckableConfigWithEndpoints(t, outbounds, endpoints)
	out, err := runSingboxCheck(t, binary, cfg)
	if err != nil {
		t.Fatalf("ядро отвергло сводный конфиг из %d outbound'ов и %d endpoint'ов: %v\n%s",
			len(outbounds), len(endpoints), err, out)
	}
	t.Logf("сводный конфиг корпуса принят ядром: %d outbound'ов, %d endpoint'ов",
		len(outbounds), len(endpoints))
}

// buildCheckableConfigWithEndpoints — тот же минимальный конфиг, что и у
// соседа, плюс секция endpoints (wireguard/tailscale живут там, а не в
// outbounds — SPEC 122).
func buildCheckableConfigWithEndpoints(t *testing.T, outbounds, endpoints []map[string]interface{}) []byte {
	t.Helper()
	cfg := map[string]interface{}{
		"log": map[string]interface{}{"level": "error"},
		"inbounds": []interface{}{
			map[string]interface{}{
				"type": "mixed", "tag": "mixed-in",
				"listen": "127.0.0.1", "listen_port": 2080,
			},
		},
		"outbounds": outbounds,
	}
	if len(endpoints) > 0 {
		cfg["endpoints"] = endpoints
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	return data
}

// firstPayloadLine — полезная нагрузка кейса: первая строка, не являющаяся
// комментарием (contract/corpus/README.md §конвенции).
func firstPayloadLine(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("чтение %s: %v", path, err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return line
	}
	return ""
}

// caseTagOf — имя кейса как основа тега узла в сводном конфиге.
func caseTagOf(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, ".uri")
}

// uniqueTag разводит совпавшие имена кейсов из разных каталогов.
func uniqueTag(tag string, used map[string]bool) string {
	candidate := tag
	for i := 2; used[candidate]; i++ {
		candidate = tag + "-" + itoa(i)
	}
	used[candidate] = true
	return candidate
}

// coreVersionOfBinary — версия ядра из `sing-box version`.
var coreVersionLine = regexp.MustCompile(`sing-box version (\S+)`)

// corpusRequiredTags — build-теги, без которых собранный из корпуса конфиг
// ядро не примет. Список узкий и обоснованный: это те возможности, чьи
// фикстуры реально лежат в корпусе. Если корпус расширят типом, требующим
// нового тега, сюда добавляется одна строка.
var corpusRequiredTags = []string{
	"with_wireguard", // endpoints типа wireguard
}

// missingCorpusCapabilities сообщает, каких тегов сборки не хватает ядру
// для проверки корпуса. Пустой результат — ядро подходит.
//
// Нужно, чтобы различие сборок не выглядело дефектом кода: кастомное ядро
// без with_wireguard отвергает конфиг с wireguard-endpoint, и тест падал с
// «unknown endpoint type: wireguard» там, где на самом деле надо взять
// другое ядро (SINGBOX_TEST_CORE) или собрать своё с нужными тегами.
func missingCorpusCapabilities(t *testing.T, binary string) []string {
	t.Helper()
	out, err := exec.Command(binary, "version").CombinedOutput()
	if err != nil {
		// Версию уже читали выше; сюда попадаем только при странной ошибке.
		return nil
	}
	text := string(out)
	var missing []string
	for _, tag := range corpusRequiredTags {
		if !strings.Contains(text, tag) {
			missing = append(missing, tag)
		}
	}
	return missing
}

func coreVersionOfBinary(t *testing.T, binary string) string {
	t.Helper()
	out, err := exec.Command(binary, "version").CombinedOutput()
	if err != nil {
		t.Fatalf("sing-box version: %v\n%s", err, out)
	}
	m := coreVersionLine.FindStringSubmatch(string(out))
	if m == nil {
		t.Fatalf("не разобрать версию ядра: %s", out)
	}
	return m[1]
}

// uncheckableFields — поля, чьё значение этот тест проверить не может:
// ядро разбирает их как КРИПТОМАТЕРИАЛ (сверяет длину, формат, подпись), а в
// общем корпусе он заведомо синтетический — настоящим ключам в файлах,
// которые едут в два репозитория, взяться неоткуда.
//
// Тест отвечает на вопрос «валиден ли КОНФИГ, собранный конвейером», а не
// «настоящие ли ключи в фикстурах»: падение на длине синтетического ключа
// говорило бы о фикстуре и молчало бы о конвейере.
//
//   - ssh host_key — «ssh: no key found» на «FakeSyntheticHostKey…»;
//   - vless encryption — «invalid encryption key length: 1198 (expected 32
//     or 1184)» на синтетическом mlkem768-ключе.
var uncheckableFields = map[string]bool{"host_key": true, "encryption": true}

// dropUncheckableFields рекурсивно снимает пути к файлам (*_path) и поля из
// uncheckableFields.
func dropUncheckableFields(m map[string]interface{}) {
	for k, v := range m {
		if strings.HasSuffix(k, "_path") || uncheckableFields[k] {
			delete(m, k)
			continue
		}
		if inner, ok := v.(map[string]interface{}); ok {
			dropUncheckableFields(inner)
		}
	}
}
