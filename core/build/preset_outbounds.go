// Package build — File preset_outbounds.go.
//
// SPEC 057-R-N (current): preset outbound binding живёт в state directly
// через Ref/Updates fields на Direction. Runtime path использует
// SyncOutboundsWithActivePresets (sync_outbounds.go) +
// MergeOutboundUpdatesInPlace (resolve_outbounds.go).
//
// Этот файл оставляет вспомогательные helper'ы:
//   - ExpandPresetOutbounds — конвертит template.PresetOutbound[i] →
//     []presetOutboundEntry (vars-substitution + if/if_or filter).
//     Используется sync_outbounds.go для расчёта expected add/update entries.
//   - applyOutboundUpdate — типизированный field-merge patch'а в target.
//     Используется resolve_outbounds.go::mergeOutboundUpdates.
//   - CleanDanglingOutboundsInRouteRules — cleanup route.rules при отсутствии
//     target outbound (e.g. preset disabled, dropped its add).
//   - cloneOptions / unionStringList — internal helpers для applyOutboundUpdate.
package build

import (
	"encoding/json"
	"fmt"
	"strings"

	"singbox-launcher/core/config/configtypes"
	"singbox-launcher/core/template"
)

// outboundSentinelLiterals — теги, которые типично преобразуются upstream
// в action-based rule (reject/block) или sing-box well-known outbound'ы
// (direct/dns-out). Если они оказываются в rule.outbound — НЕ считаем
// dangling даже если их нет в финальном outbounds[].
//
// Защита от ложных drop'ов: preset/user может эмитить rule с outbound:"reject"
// напрямую (без прохождения через outboundutil.ApplyOutboundToRule),
// и cleanup не должен убирать такие rule из конфига.
var outboundSentinelLiterals = map[string]bool{
	"reject":  true,
	"block":   true,
	"drop":    true,
	"direct":  true,
	"dns-out": true,
}

// presetOutboundEntry — internal: разделяет режим применения и сам
// configtypes.Direction (без control-полей mode/if/if_or).
//
// Возвращается ExpandPresetOutbounds и потребляется sync_outbounds.go.
// PresetID нужен для warning'ов и для разрешения origin'а в Updates[] стеке.
type presetOutboundEntry struct {
	Mode     string // "add" | "update"
	Config   configtypes.Direction
	PresetID string
}

// ExpandPresetOutbounds разворачивает preset.Outbounds[] в []presetOutboundEntry
// с уже применённой substitution @var и if/if_or фильтрацией.
//
// userVars — значения переменных из state.rule.body.vars (только diff от
// template default'ов; пустые/отсутствующие резолвятся через
// preset.vars[].Default).
//
// Алгоритм идентичен ExpandPreset (rule/rule_set/dns_rule path), но для
// каждой entry дополнительно:
//   - normalizes Mode ("" → "add"; loader уже зачистил unknown);
//   - JSON round-trip через map для substitute @var;
//   - drop control-полей (mode/if/if_or) из map ДО unmarshal в Direction;
//   - типизированный re-unmarshal в configtypes.Direction.
//
// На unresolved @var — entry skip + warning, остальные entries продолжают
// обрабатываться (в отличие от ExpandPreset который отменяет весь preset
// на unresolved — там dangling @var в rule_set/rule может всё разломать,
// здесь же одна сломанная entry не блокирует другие).
func ExpandPresetOutbounds(preset *template.Preset, userVars map[string]string, target template.TargetSpec) ([]presetOutboundEntry, []ExpandWarning) {
	if preset == nil || len(preset.Outbounds) == 0 {
		return nil, nil
	}

	// === 1. Build varsMap (тот же паттерн что в ExpandPreset). ===
	varsMap := make(map[string]string, len(preset.Vars))
	for _, v := range preset.Vars {
		if userVal, ok := userVars[v.Name]; ok && userVal != "" {
			varsMap[v.Name] = userVal
		} else {
			varsMap[v.Name] = v.Default
		}
	}

	// === 2. Filter vars по if/if_or; неактивные → удалить из map. ===
	activeVars := filterActiveVars(preset.Vars, varsMap, target)
	for name := range varsMap {
		if !activeVars[name] {
			delete(varsMap, name)
		}
	}

	var warnings []ExpandWarning
	out := make([]presetOutboundEntry, 0, len(preset.Outbounds))

	for i := range preset.Outbounds {
		ob := preset.Outbounds[i]

		// === 3. Filter by entry if/if_or. ===
		if !evalIf(ob.If, ob.IfOr, varsMap) {
			continue
		}

		mode := ob.Mode
		if mode == "" {
			mode = "add"
		}

		// === 4. Marshal → map → substitute → strip control → unmarshal. ===
		raw, err := json.Marshal(ob)
		if err != nil {
			warnings = append(warnings, ExpandWarning{
				PresetID: preset.ID,
				Message:  fmt.Sprintf("outbounds[%d] (tag=%q): marshal: %v", i, ob.Tag, err),
			})
			continue
		}
		var asMap map[string]interface{}
		if err := json.Unmarshal(raw, &asMap); err != nil {
			warnings = append(warnings, ExpandWarning{
				PresetID: preset.ID,
				Message:  fmt.Sprintf("outbounds[%d] (tag=%q): unmarshal: %v", i, ob.Tag, err),
			})
			continue
		}
		substituted, subWarns, ok := substitutePresetBody(asMap, preset.Vars, nil, varsMap, target)
		warnings = append(warnings, substitutionWarnings(preset.ID, subWarns)...)
		if !ok {
			warnings = append(warnings, ExpandWarning{
				PresetID: preset.ID,
				Message: fmt.Sprintf(
					"outbounds[%d] (tag=%q): substitution failed (entry skipped)",
					i, ob.Tag),
			})
			continue
		}
		substMap, _ := substituted.(map[string]interface{})
		if substMap == nil {
			continue
		}
		// Control-поля не должны попасть в configtypes.Direction
		// (он их и не имеет — strict-decoder бы ругался; но native генератор
		// потом маршалит Direction обратно через json.Marshal, где
		// неизвестные ключи не возникают, так что strip — defensive).
		delete(substMap, "mode")
		delete(substMap, "if")
		delete(substMap, "if_or")

		finalRaw, err := json.Marshal(substMap)
		if err != nil {
			warnings = append(warnings, ExpandWarning{
				PresetID: preset.ID,
				Message: fmt.Sprintf(
					"outbounds[%d] (tag=%q): re-marshal: %v", i, ob.Tag, err),
			})
			continue
		}
		var oc configtypes.Direction
		if err := json.Unmarshal(finalRaw, &oc); err != nil {
			warnings = append(warnings, ExpandWarning{
				PresetID: preset.ID,
				Message: fmt.Sprintf(
					"outbounds[%d] (tag=%q): re-unmarshal to Direction: %v",
					i, ob.Tag, err),
			})
			continue
		}
		out = append(out, presetOutboundEntry{
			Mode:     mode,
			Config:   oc,
			PresetID: preset.ID,
		})
	}
	return out, warnings
}

// applyOutboundUpdate — типизированный field-merge patch'а в target.
//
// Возвращает НОВУЮ структуру (target value-immutable; AddOutbounds/Options/
// Filters/PreferredDefault внутри — fresh-allocated через cloneOptions/union).
//
// Семантика полей (см. PresetOutbound docstring):
//   - Type, Tag    — НЕ меняются (immutable; Type loader уже зачищает для update)
//   - Filters      — replace целиком (если patch.Filters != nil)
//   - AddOutbounds — union (preserve order, dedupe); для USER patch список
//     затем заменяется целиком в applyOutboundUpdatePatch (см. userPatch)
//   - Options.*    — per-key replace в target.Options (нет глубокого merge);
//     nil-значение = "оверрайда нет" → key берётся из target (issue #91)
//   - PreferredDefault — replace
//   - Wizard       — replace
//   - Comment      — replace iff patch.Comment != ""
//   - Label        — replace iff patch.Label != "" (SPEC 104)
//   - Auto         — replace целиком iff patch.Auto != nil (SPEC 104)
func applyOutboundUpdate(target, patch configtypes.Direction) configtypes.Direction {
	out := target

	if patch.Filters != nil {
		out.Filters = cloneOptions(patch.Filters)
	}
	if len(patch.AddOutbounds) > 0 {
		out.AddOutbounds = unionStringList(target.AddOutbounds, patch.AddOutbounds)
	}
	if len(patch.Options) > 0 {
		merged := cloneOptions(target.Options)
		if merged == nil {
			merged = make(map[string]interface{}, len(patch.Options))
		}
		for k, v := range patch.Options {
			// nil == "нет оверрайда для этого ключа" — значение берётся из base.
			// Историческая причина (issue #91): mapDiff писал nil как tombstone
			// "ключ удалён", merge копировал его как живое значение, и emitter
			// выдавал "interval":null → sing-box падал с invalid duration "".
			// Такие null'ы залипли в state.json у пользователей, поэтому чтение
			// обязано их игнорировать, а не переносить в merged.
			if v == nil {
				continue
			}
			merged[k] = v
		}
		out.Options = merged
	}
	if patch.PreferredDefault != nil {
		out.PreferredDefault = cloneOptions(patch.PreferredDefault)
	}
	if patch.Comment != "" {
		out.Comment = patch.Comment
	}
	// Двойник заменяется целиком: его поля связаны между собой (режим и
	// параметры пула), и по-полевой merge собрал бы round_robin без пула.
	if patch.Auto != nil {
		out.Auto = patch.Auto
	}
	// Disabled приходит через map-патч (см. applyOutboundUpdatePatch): там
	// отсутствие ключа отличимо от false, а в типизированной форме — нет.
	return out
}

// cloneOptions — deep-copy map[string]interface{} через JSON round-trip.
// nil-вход → nil-выход (без аллокации пустого map'а — caller проверяет).
func cloneOptions(m map[string]interface{}) map[string]interface{} {
	if m == nil {
		return nil
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	var out map[string]interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// CleanDanglingOutboundsInRouteRules чистит rule.outbound ссылки в
// route.rules[], которых нет в finalTags (т.е. tag отсутствует в финальном
// config.outbounds[]).
//
// Сценарий: юзер сохранил кастом-rule с outbound="ru VPN 🇷🇺", потом
// disable ru-inside preset → tag "ru VPN 🇷🇺" исчез из outbounds[]. Без
// cleanup'а sing-box валится на unknown outbound. Cleanup либо подменяет
// на fallback (если непустой и сам валидный), либо drop'ает rule.
//
// Параметры:
//   - routeRaw — раскрытая route-секция (после MergeRouteSection + MergePresetsIntoRoute);
//   - finalTags — set всех outbound-тегов которые попадут в финальный config;
//   - fallback — обычно route.final; non-empty И ∈ finalTags → подменяется,
//     иначе rule drop'ается.
//
// Возвращает (newRoute, warnings, err). routeRaw НЕ мутируется.
// Sentinel-теги (reject/block/drop/direct/dns-out) и rules без outbound
// (action-based) пропускаются без изменений.
//
// Skip в preview mode — caller отвечает за условие (наследие 0c3dce5 / P8:
// ctx.Cache в preview может быть неполный, false-positive dangling).
func CleanDanglingOutboundsInRouteRules(routeRaw json.RawMessage, finalTags map[string]bool, fallback string) (json.RawMessage, []string, error) {
	if len(routeRaw) == 0 {
		return routeRaw, nil, nil
	}
	var route map[string]interface{}
	if err := json.Unmarshal(routeRaw, &route); err != nil {
		return nil, nil, fmt.Errorf("clean dangling outbounds: parse route: %w", err)
	}
	rulesRaw, _ := route["rules"].([]interface{})
	if len(rulesRaw) == 0 {
		return routeRaw, nil, nil
	}

	var warnings []string
	// Recursive: logical rules nest arbitrarily, and a dangling target one level
	// down was previously invisible to both this cleaner and the validator — the
	// incident class surviving one nesting level deeper.
	cleanedRules, err := cleanRuleList(rulesRaw, "route.rules", finalTags, fallback, &warnings)
	if err != nil {
		return nil, warnings, err
	}
	if len(cleanedRules) > 0 {
		route["rules"] = cleanedRules
	} else {
		delete(route, "rules")
	}

	out, err := json.MarshalIndent(route, "", "  ")
	if err != nil {
		return nil, warnings, fmt.Errorf("clean dangling outbounds: marshal: %w", err)
	}
	return out, warnings, nil
}

// cleanRuleList — рекурсивная очистка списка правил (включая logical-подправила).
//
// Возвращает очищенный список; warning'и копятся в *warnings.
func cleanRuleList(rules []interface{}, path string, finalTags map[string]bool, fallback string, warnings *[]string) ([]interface{}, error) {
	kept := make([]interface{}, 0, len(rules))
	for i, item := range rules {
		m, ok := item.(map[string]interface{})
		if !ok {
			// Не-объект rule — пропускаем как есть (defensive).
			kept = append(kept, item)
			continue
		}
		rulePath := fmt.Sprintf("%s[%d]", path, i)
		cleaned, err := cleanDanglingOutboundRefInRule(m, rulePath, finalTags, fallback, warnings)
		if err != nil {
			return nil, err
		}
		// Recurse into logical sub-rules, preserving any rewrite of the parent.
		if sub, ok := cleaned["rules"].([]interface{}); ok {
			subCleaned, err := cleanRuleList(sub, rulePath+".rules", finalTags, fallback, warnings)
			if err != nil {
				return nil, err
			}
			cleaned = cloneRule(cleaned)
			cleaned["rules"] = subCleaned
		}
		kept = append(kept, cleaned)
	}
	return kept, nil
}

// cleanDanglingOutboundRefInRule — per-rule cleanup.
//
// Returns the rule to keep, or an error when the rule cannot be made valid.
//
// A dangling target with NO valid fallback used to DROP the rule and log a
// warning. That silently deleted a routing decision the user had made — traffic
// meant for one outbound quietly fell through to route.final instead — and it
// contradicted the project's own fail-closed policy for a dangling `detour`,
// which drops the node rather than silently rerouting it. A warning in a log is
// not informed consent, so the situation is now a build error the user can act
// on, exactly like an unrepairable route.final.
//
// Pure: не мутирует rule на месте — clone'ит когда нужно подменить outbound.
func cleanDanglingOutboundRefInRule(rule map[string]interface{}, path string, finalTags map[string]bool, fallback string, warnings *[]string) (map[string]interface{}, error) {
	outRaw, has := rule["outbound"]
	if !has {
		return rule, nil // action-based rule (reject/block через "action") — не трогаем
	}
	outStr, _ := outRaw.(string)
	if outStr == "" {
		return rule, nil
	}
	if outboundSentinelLiterals[outStr] {
		return rule, nil // reject/block/direct/dns-out literal — преобразуется upstream
	}
	if finalTags[outStr] {
		return rule, nil // valid
	}

	// Dangling. Подменяем на fallback если он сам валидный.
	if fallback != "" && finalTags[fallback] {
		warn := fmt.Sprintf(
			"%s: dangling outbound %q → replaced with fallback %q",
			path, outStr, fallback)
		if warnings != nil {
			*warnings = append(*warnings, warn)
		}
		cloned := cloneRule(rule)
		cloned["outbound"] = fallback
		return cloned, nil
	}
	// No fallback can stand in. Failing beats silently rerouting the user's
	// traffic; the message names the rule so it can be found and fixed.
	return nil, fmt.Errorf(
		"%s targets outbound %q, which does not exist, and no valid fallback is available to replace it with",
		path, outStr)
}

// cloneRule — поверхностная копия rule, чтобы правки не текли в исходный объект.
func cloneRule(rule map[string]interface{}) map[string]interface{} {
	cloned := make(map[string]interface{}, len(rule))
	for k, v := range rule {
		cloned[k] = v
	}
	return cloned
}

// collectAllFinalOutboundTags — set всех outbound-тегов, которые попадут
// в финальный `config.outbounds[]`.
//
// Источники:
//   - cfg["outbounds"] — template static outbounds (direct-out etc.) ПОСЛЕ
//     applied preset.outbounds pre-patch'а (потому что preset.outbounds
//     mode=add цели тоже идут через native generator → попадают в Cache).
//   - ctx.Cache.Outbounds — все parser-generated outbounds (proxy-out,
//     auto-proxy-out, vpn ①/②, ноды с tag_prefix'ами, добавленные preset'ом
//     "ru VPN 🇷🇺", etc.).
//
// Sentinel-теги (reject/block/drop) НЕ включены — они обрабатываются отдельно
// в cleanDanglingOutboundRefInRule через outboundSentinelLiterals lookup.
func collectAllFinalOutboundTags(ctx BuildContext, cfg map[string]json.RawMessage) map[string]bool {
	tags := make(map[string]bool, 32)

	if raw, ok := cfg["outbounds"]; ok && len(raw) > 0 {
		var arr []map[string]interface{}
		if err := json.Unmarshal(raw, &arr); err == nil {
			for _, ob := range arr {
				if tag, _ := ob["tag"].(string); tag != "" {
					tags[tag] = true
				}
			}
		}
	}
	if ctx.Cache != nil {
		// Endpoints тоже в set: sing-box резолвит group members / route
		// outbounds / detour и через endpoint manager, так что WG-endpoint —
		// легитимная цель ссылки, а не dangling.
		for _, list := range [][]json.RawMessage{ctx.Cache.Outbounds, ctx.Cache.Endpoints} {
			for _, raw := range list {
				// Legacy cache-entries могут нести `// comment`-префикс перед
				// JSON — без среза префикса Unmarshal падает и тег теряется.
				_, jsonPart := splitEntryComment(string(raw))
				var ob map[string]interface{}
				if err := json.Unmarshal([]byte(strings.TrimRight(strings.TrimSpace(jsonPart), ",")), &ob); err != nil {
					continue
				}
				if tag, _ := ob["tag"].(string); tag != "" {
					tags[tag] = true
				}
			}
		}
	}
	return tags
}

// unionStringList — union двух []string preserving первое вхождение
// (a-первый, b-второй); dedup case-sensitive.
//
// Используется для merge'а AddOutbounds в mode=update: preset.AddOutbounds
// добавляются ПОСЛЕ target.AddOutbounds (template-defined идут первыми,
// preset-added — после). Это сохраняет стабильность ordering'а в UI selector'е.
func unionStringList(a, b []string) []string {
	seen := make(map[string]bool, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, s := range a {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, s := range b {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
