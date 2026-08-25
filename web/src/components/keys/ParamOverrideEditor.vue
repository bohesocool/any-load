<script setup lang="ts">
import { Add, Remove } from "@vicons/ionicons5";
import {
  NButton,
  NIcon,
  NInput,
  NSelect,
  NSwitch,
  NTooltip,
} from "naive-ui";
import { computed, onMounted, ref, watch } from "vue";
import { useI18n } from "vue-i18n";

// --- types ---

interface ParamOverrideCondition {
  path: string;
  mode: string;
  value: string; // raw text, parsed loosely on submit
  invert: boolean;
  pass_missing_key: boolean;
}

interface ParamOverrideOperation {
  description: string;
  path: string;
  mode: string;
  value: string;
  from: string;
  to: string;
  keep_origin: boolean;
  logic: string;
  conditions: ParamOverrideCondition[];
}

interface Props {
  /** The backend config object: { operations: [...] } or a legacy flat map. */
  value: Record<string, unknown> | undefined;
}

interface Emits {
  (e: "update:value", value: Record<string, unknown>): void;
}

const props = defineProps<Props>();
const emit = defineEmits<Emits>();
const { t } = useI18n();

// --- mode metadata (which fields each mode shows), aligned with new-api ---

const MODE_META: Record<string, { path?: boolean; value?: boolean; from?: boolean; to?: boolean; keepOrigin?: boolean }> = {
  delete: { path: true },
  set: { path: true, value: true, keepOrigin: true },
  append: { path: true, value: true, keepOrigin: true },
  prepend: { path: true, value: true, keepOrigin: true },
  copy: { from: true, to: true },
  move: { from: true, to: true },
};

// --- template presets (only the ones the 6-mode engine can run) ---

const OPERATION_TEMPLATE = {
  operations: [
    {
      description: "Set default temperature for gpt-* models.",
      path: "temperature",
      mode: "set",
      value: 0.7,
      conditions: [{ path: "model", mode: "prefix", value: "gpt-" }],
      logic: "AND",
    },
  ],
};
const LEGACY_TEMPLATE = { temperature: 0, max_tokens: 1000 };
const DELETE_USER_PREFILL = {
  operations: [
    {
      description: "Remove the user field before relaying upstream.",
      path: "user",
      mode: "delete",
    },
  ],
};
const COPY_MODEL_TO_META = {
  operations: [
    {
      description: "Copy model into metadata.model for logging.",
      mode: "copy",
      from: "model",
      to: "metadata.model",
    },
  ],
};

const TEMPLATE_PRESETS: { value: string; kind: "operations" | "legacy" }[] = [
  { value: "operations_default", kind: "operations" },
  { value: "legacy_default", kind: "legacy" },
  { value: "delete_field", kind: "operations" },
  { value: "copy_field", kind: "operations" },
];

const TEMPLATE_PAYLOADS: Record<string, Record<string, unknown>> = {
  operations_default: OPERATION_TEMPLATE,
  legacy_default: LEGACY_TEMPLATE,
  delete_field: DELETE_USER_PREFILL,
  copy_field: COPY_MODEL_TO_META,
};

// --- value <-> text round trip (new-api parseLooseValue / toValueText) ---

function parseLooseValue(text: string): unknown {
  const s = text.trim();
  if (s === "") return "";
  try {
    return JSON.parse(s);
  } catch {
    return s;
  }
}

function valueToText(v: unknown): string {
  if (v === undefined || v === null) return "";
  if (typeof v === "string") return v;
  return JSON.stringify(v);
}

// --- normalize / factories ---

const VALID_MODES = new Set(["set", "delete", "copy", "move", "append", "prepend"]);
const VALID_COND_MODES = new Set(["full", "prefix", "suffix", "contains", "gt", "gte", "lt", "lte"]);

function normalizeCondition(c: Record<string, unknown> = {}): ParamOverrideCondition {
  return {
    path: typeof c.path === "string" ? c.path : "",
    mode: VALID_COND_MODES.has(c.mode as string) ? (c.mode as string) : "full",
    value: valueToText(c.value),
    invert: c.invert === true,
    pass_missing_key: c.pass_missing_key === true,
  };
}

function normalizeOperation(op: Record<string, unknown> = {}): ParamOverrideOperation {
  return {
    description: typeof op.description === "string" ? op.description : "",
    path: typeof op.path === "string" ? op.path : "",
    mode: VALID_MODES.has(op.mode as string) ? (op.mode as string) : "set",
    value: valueToText(op.value),
    from: typeof op.from === "string" ? op.from : "",
    to: typeof op.to === "string" ? op.to : "",
    keep_origin: op.keep_origin === true,
    logic: String(op.logic || "OR").toUpperCase() === "AND" ? "AND" : "OR",
    conditions: Array.isArray(op.conditions) ? (op.conditions as Record<string, unknown>[]).map(normalizeCondition) : [],
  };
}

function createDefaultOperation(): ParamOverrideOperation {
  return normalizeOperation({ mode: "set" });
}

// An operation is "blank" if it's a set with nothing filled in (filtered out on save).
function isOperationBlank(op: ParamOverrideOperation): boolean {
  const hasCondition = op.conditions.some(c => c.path.trim() || c.value.trim() || c.mode !== "full" || c.invert || c.pass_missing_key);
  return (
    op.mode === "set" &&
    !op.path.trim() &&
    !op.from.trim() &&
    !op.to.trim() &&
    op.value.trim() === "" &&
    !op.keep_origin &&
    !hasCondition
  );
}

// --- state: dual mode (visual / json) + visual sub-mode (operations / legacy) ---

const editMode = ref<"visual" | "json">("visual");
const visualMode = ref<"operations" | "legacy">("operations");
const operations = ref<ParamOverrideOperation[]>([createDefaultOperation()]);
const legacyValue = ref("");
const jsonText = ref("");
const jsonError = ref("");
const templatePresetKey = ref("operations_default");

// Load incoming value into editor state.
function parseInitialState(raw: Record<string, unknown> | undefined) {
  if (!raw || typeof raw !== "object" || Object.keys(raw).length === 0) {
    editMode.value = "visual";
    visualMode.value = "operations";
    operations.value = [createDefaultOperation()];
    legacyValue.value = "";
    jsonText.value = "";
    jsonError.value = "";
    return;
  }
  if (Array.isArray((raw as Record<string, unknown>).operations)) {
    const opsRaw = (raw as Record<string, unknown>).operations as Record<string, unknown>[];
    editMode.value = "visual";
    visualMode.value = "operations";
    operations.value = opsRaw.length > 0 ? opsRaw.map(normalizeOperation) : [createDefaultOperation()];
    legacyValue.value = "";
    jsonText.value = JSON.stringify(raw, null, 2);
    jsonError.value = "";
    return;
  }
  // Legacy flat map.
  editMode.value = "visual";
  visualMode.value = "legacy";
  legacyValue.value = JSON.stringify(raw, null, 2);
  operations.value = [createDefaultOperation()];
  jsonText.value = JSON.stringify(raw, null, 2);
  jsonError.value = "";
}

// Emit the current editor state back to the parent as a backend config object.
function serialize(): Record<string, unknown> {
  if (editMode.value === "json") {
    const trimmed = jsonText.value.trim();
    if (!trimmed) return {};
    try {
      return JSON.parse(trimmed) as Record<string, unknown>;
    } catch {
      return {};
    }
  }
  if (visualMode.value === "legacy") {
    const trimmed = legacyValue.value.trim();
    if (!trimmed) return {};
    try {
      const parsed = JSON.parse(trimmed);
      if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) return parsed as Record<string, unknown>;
    } catch {
      /* ignore */
    }
    return {};
  }
  const valid = operations.value.filter(o => !isOperationBlank(o));
  if (valid.length === 0) return {};
  return {
    operations: valid.map(o => {
      const meta = MODE_META[o.mode] || MODE_META.set;
      const payload: Record<string, unknown> = { mode: o.mode };
      if (o.description.trim()) payload.description = o.description.trim();
      if (meta.path) payload.path = o.path.trim();
      if (meta.value) payload.value = parseLooseValue(o.value);
      if (meta.keepOrigin && o.keep_origin) payload.keep_origin = true;
      if (meta.from) payload.from = o.from.trim();
      if (meta.to) payload.to = o.to.trim();
      const conds = o.conditions
        .map(c => {
          const p = c.path.trim();
          if (!p) return null;
          const cp: Record<string, unknown> = { path: p, mode: c.mode || "full", value: parseLooseValue(c.value) };
          if (c.invert) cp.invert = true;
          if (c.pass_missing_key) cp.pass_missing_key = true;
          return cp;
        })
        .filter(Boolean);
      if (conds.length > 0) {
        payload.conditions = conds;
        payload.logic = o.logic === "AND" ? "AND" : "OR";
      }
      return payload;
    }),
  };
}

// Build the JSON text representation of the current visual state (for the JSON tab).
function buildVisualJson(): string {
  if (visualMode.value === "legacy") return legacyValue.value;
  const valid = operations.value.filter(o => !isOperationBlank(o));
  if (valid.length === 0) return "";
  return JSON.stringify({ operations: serialize().operations }, null, 2);
}

// --- mode switching ---

function switchToJsonMode() {
  if (editMode.value === "json") return;
  jsonText.value = buildVisualJson() || (visualMode.value === "legacy" ? legacyValue.value : "");
  jsonError.value = "";
  editMode.value = "json";
}

function switchToVisualMode() {
  if (editMode.value === "visual") return;
  const trimmed = jsonText.value.trim();
  if (!trimmed) {
    operations.value = [createDefaultOperation()];
    visualMode.value = "operations";
    legacyValue.value = "";
    jsonError.value = "";
    editMode.value = "visual";
    return;
  }
  let parsed: Record<string, unknown>;
  try {
    parsed = JSON.parse(trimmed) as Record<string, unknown>;
  } catch {
    jsonError.value = t("keys.paramOverrideInvalidJson");
    return;
  }
  if (parsed && typeof parsed === "object" && !Array.isArray(parsed) && Array.isArray(parsed.operations)) {
    const opsRaw = parsed.operations as Record<string, unknown>[];
    operations.value = opsRaw.length > 0 ? opsRaw.map(normalizeOperation) : [createDefaultOperation()];
    visualMode.value = "operations";
    legacyValue.value = "";
    jsonError.value = "";
    editMode.value = "visual";
    templatePresetKey.value = "operations_default";
    return;
  }
  if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
    visualMode.value = "legacy";
    legacyValue.value = JSON.stringify(parsed, null, 2);
    operations.value = [createDefaultOperation()];
    jsonError.value = "";
    editMode.value = "visual";
    templatePresetKey.value = "legacy_default";
    return;
  }
  jsonError.value = t("keys.paramOverrideMustBeObject");
}

// --- templates ---

const templateOptions = computed(() =>
  TEMPLATE_PRESETS.map(p => ({ label: t(`keys.paramOverrideTemplate_${p.value}`), value: p.value }))
);

function fillTemplate() {
  const payload = TEMPLATE_PAYLOADS[templatePresetKey.value];
  if (!payload) return;
  if (Array.isArray(payload.operations)) {
    operations.value = (payload.operations as Record<string, unknown>[]).map(normalizeOperation);
    visualMode.value = "operations";
    legacyValue.value = "";
  } else {
    visualMode.value = "legacy";
    legacyValue.value = JSON.stringify(payload, null, 2);
    operations.value = [createDefaultOperation()];
  }
  jsonError.value = "";
  editMode.value = "visual";
}

function appendTemplate() {
  const payload = TEMPLATE_PAYLOADS[templatePresetKey.value];
  if (!payload) return;
  if (Array.isArray(payload.operations)) {
    const appended = (payload.operations as Record<string, unknown>[]).map(normalizeOperation);
    const existing = visualMode.value === "operations" ? operations.value.filter(o => !isOperationBlank(o)) : [];
    operations.value = [...existing, ...appended];
    visualMode.value = "operations";
    legacyValue.value = "";
  } else if (visualMode.value === "legacy") {
    let current: Record<string, unknown> = {};
    try {
      const p = JSON.parse(legacyValue.value.trim());
      if (p && typeof p === "object") current = p as Record<string, unknown>;
    } catch {
      /* ignore */
    }
    legacyValue.value = JSON.stringify({ ...(payload as Record<string, unknown>), ...current }, null, 2);
  }
  jsonError.value = "";
  editMode.value = "visual";
}

function resetEditor() {
  operations.value = [createDefaultOperation()];
  visualMode.value = "operations";
  legacyValue.value = "";
  jsonText.value = "";
  jsonError.value = "";
  templatePresetKey.value = "operations_default";
  editMode.value = "visual";
}

// --- row helpers ---

function addOperation() {
  operations.value.push(createDefaultOperation());
}
function removeOperation(index: number) {
  operations.value.splice(index, 1);
}
function addCondition(opIndex: number) {
  operations.value[opIndex].conditions.push({
    path: "",
    mode: "full",
    value: "",
    invert: false,
    pass_missing_key: false,
  });
}
function removeCondition(opIndex: number, condIndex: number) {
  operations.value[opIndex].conditions.splice(condIndex, 1);
}

// --- live validation for JSON mode ---

function onJsonChange(v: string) {
  jsonText.value = v;
  const trimmed = v.trim();
  if (!trimmed) {
    jsonError.value = "";
    return;
  }
  try {
    JSON.parse(trimmed);
    jsonError.value = "";
  } catch {
    jsonError.value = t("keys.paramOverrideJsonFormatError");
  }
}

// --- mode label helpers ---

const operationModeOptions = computed(() => [
  { label: t("keys.operationModeSet"), value: "set" },
  { label: t("keys.operationModeDelete"), value: "delete" },
  { label: t("keys.operationModeCopy"), value: "copy" },
  { label: t("keys.operationModeMove"), value: "move" },
  { label: t("keys.operationModeAppend"), value: "append" },
  { label: t("keys.operationModePrepend"), value: "prepend" },
]);

const conditionModeOptions = computed(() => [
  { label: t("keys.conditionModeFull"), value: "full" },
  { label: t("keys.conditionModePrefix"), value: "prefix" },
  { label: t("keys.conditionModeSuffix"), value: "suffix" },
  { label: t("keys.conditionModeContains"), value: "contains" },
  { label: t("keys.conditionModeGt"), value: "gt" },
  { label: t("keys.conditionModeGte"), value: "gte" },
  { label: t("keys.conditionModeLt"), value: "lt" },
  { label: t("keys.conditionModeLte"), value: "lte" },
]);

const logicOptions = computed(() => [
  { label: t("keys.logicOr"), value: "OR" },
  { label: t("keys.logicAnd"), value: "AND" },
]);

// --- sync to parent on any state change ---

watch(
  [operations, legacyValue, jsonText, editMode, visualMode],
  () => emit("update:value", serialize()),
  { deep: true }
);

// Load the incoming value once on mount. We do NOT re-run on later parent
// writes: the parent writes back our own emitted state (v-model), so re-parsing
// it would reset in-progress edits. (Matches new-api, which only parses on open.)
onMounted(() => parseInitialState(props.value));
</script>

<template>
  <div class="param-override-editor">
    <!-- toolbar: mode toggle + templates -->
    <div class="poe-toolbar">
      <div class="poe-toolbar-group">
        <span class="poe-toolbar-label">{{ t("keys.paramOverrideMode") }}</span>
        <n-button
          size="small"
          :type="editMode === 'visual' ? 'primary' : 'default'"
          @click="switchToVisualMode"
        >
          {{ t("keys.paramOverrideVisual") }}
        </n-button>
        <n-button
          size="small"
          :type="editMode === 'json' ? 'primary' : 'default'"
          @click="switchToJsonMode"
        >
          {{ t("keys.paramOverrideJson") }}
        </n-button>
      </div>

      <div class="poe-toolbar-group">
        <span class="poe-toolbar-label">{{ t("keys.paramOverrideTemplate") }}</span>
        <n-select
          v-model:value="templatePresetKey"
          :options="templateOptions"
          size="small"
          style="width: 200px"
        />
        <n-button size="small" @click="fillTemplate">{{ t("keys.paramOverrideFillTemplate") }}</n-button>
        <n-button size="small" quaternary @click="appendTemplate">{{ t("keys.paramOverrideAppendTemplate") }}</n-button>
        <n-button size="small" quaternary @click="resetEditor">{{ t("keys.paramOverrideReset") }}</n-button>
      </div>
    </div>

    <!-- VISUAL MODE -->
    <div v-if="editMode === 'visual'">
      <!-- operations visual sub-mode -->
      <template v-if="visualMode === 'operations'">
        <div v-if="operations.length === 0" class="poe-empty">
          {{ t("keys.paramOverrideNoRules") }}
        </div>
        <div
          v-for="(op, index) in operations"
          :key="index"
          class="poe-op-row"
        >
          <div class="poe-op-head">
            <span class="poe-op-index">#{{ index + 1 }}</span>
            <n-input
              v-model:value="op.description"
              :placeholder="t('keys.operationDescription')"
              size="small"
              class="poe-op-desc"
            />
            <n-button
              @click="removeOperation(index)"
              type="error"
              quaternary
              circle
              size="small"
            >
              <template #icon><n-icon :component="Remove" /></template>
            </n-button>
          </div>

          <div class="poe-op-line">
            <n-select
              v-model:value="op.mode"
              :options="operationModeOptions"
              :placeholder="t('keys.selectOperationMode')"
              size="small"
              style="flex: 0 0 140px"
            />
            <!-- copy / move: from + to -->
            <template v-if="op.mode === 'copy' || op.mode === 'move'">
              <n-input v-model:value="op.from" :placeholder="t('keys.operationFrom')" size="small" />
              <n-input v-model:value="op.to" :placeholder="t('keys.operationTo')" size="small" />
            </template>
            <!-- set/delete/append/prepend: path -->
            <n-input
              v-else
              v-model:value="op.path"
              :placeholder="t('keys.operationPath')"
              size="small"
            />
            <!-- value field for set/append/prepend -->
            <n-input
              v-if="op.mode === 'set' || op.mode === 'append' || op.mode === 'prepend'"
              v-model:value="op.value"
              :placeholder="t('keys.operationValue')"
              size="small"
            />
            <!-- keep_origin switch -->
            <n-tooltip
              v-if="op.mode === 'set' || op.mode === 'append' || op.mode === 'prepend'"
              trigger="hover"
              placement="top"
            >
              <template #trigger>
                <div class="poe-keep-origin">
                  <n-switch v-model:checked="op.keep_origin" size="small" />
                  <span class="poe-keep-origin-label">{{ t("keys.keepOrigin") }}</span>
                </div>
              </template>
              {{ t("keys.keepOriginTooltip") }}
            </n-tooltip>
          </div>

          <!-- conditions -->
          <div class="poe-conditions">
            <div v-if="op.conditions.length > 0" class="poe-cond-head">
              <span class="poe-cond-label">{{ t("keys.conditions") }}</span>
              <n-select
                v-model:value="op.logic"
                :options="logicOptions"
                size="small"
                style="flex: 0 0 110px"
              />
            </div>
            <div
              v-for="(cond, cIndex) in op.conditions"
              :key="cIndex"
              class="poe-cond-line"
            >
              <n-select
                v-model:value="cond.mode"
                :options="conditionModeOptions"
                size="small"
                style="flex: 0 0 120px"
              />
              <n-input v-model:value="cond.path" :placeholder="t('keys.conditionPath')" size="small" />
              <n-input v-model:value="cond.value" :placeholder="t('keys.conditionValue')" size="small" />
              <n-tooltip trigger="hover" placement="top">
                <template #trigger>
                  <div class="poe-cond-switch">
                    <n-switch v-model:checked="cond.invert" size="small" />
                    <span>{{ t("keys.invert") }}</span>
                  </div>
                </template>
                {{ t("keys.invertTooltip") }}
              </n-tooltip>
              <n-tooltip trigger="hover" placement="top">
                <template #trigger>
                  <div class="poe-cond-switch">
                    <n-switch v-model:checked="cond.pass_missing_key" size="small" />
                    <span>{{ t("keys.passMissingKey") }}</span>
                  </div>
                </template>
                {{ t("keys.passMissingKeyTooltip") }}
              </n-tooltip>
              <n-button
                @click="removeCondition(index, cIndex)"
                type="error"
                quaternary
                circle
                size="small"
              >
                <template #icon><n-icon :component="Remove" /></template>
              </n-button>
            </div>
            <n-button @click="addCondition(index)" dashed size="small" style="width: 100%">
              <template #icon><n-icon :component="Add" /></template>
              {{ t("keys.addCondition") }}
            </n-button>
            <div v-if="op.conditions.length === 0" class="poe-cond-hint">
              {{ t("keys.paramOverrideAlwaysExecutes") }}
            </div>
          </div>
        </div>

        <n-button @click="addOperation" dashed style="width: 100%">
          <template #icon><n-icon :component="Add" /></template>
          {{ t("keys.addParamOverride") }}
        </n-button>
      </template>

      <!-- legacy visual sub-mode: raw JSON object -->
      <template v-else>
        <div class="poe-legacy-hint">{{ t("keys.paramOverrideLegacyHint") }}</div>
        <n-input
          :value="legacyValue"
          @update:value="legacyValue = $event"
          type="textarea"
          :placeholder='`{"temperature": 0.7, "max_tokens": 1000}`'
          :rows="8"
          style="font-family: monospace"
        />
      </template>
    </div>

    <!-- JSON MODE -->
    <div v-else>
      <div class="poe-json-hint">{{ t("keys.paramOverrideJsonHint") }}</div>
      <n-input
        :value="jsonText"
        @update:value="onJsonChange"
        type="textarea"
        :placeholder="JSON.stringify(OPERATION_TEMPLATE, null, 2)"
        :rows="12"
        style="font-family: monospace"
      />
      <div v-if="jsonError" class="poe-json-error">{{ jsonError }}</div>
    </div>
  </div>
</template>

<style scoped>
.param-override-editor {
  width: 100%;
}

.poe-toolbar {
  display: flex;
  flex-wrap: wrap;
  gap: 12px;
  align-items: center;
  margin-bottom: 12px;
}

.poe-toolbar-group {
  display: flex;
  align-items: center;
  gap: 8px;
}

.poe-toolbar-label {
  font-size: 13px;
  color: #666;
  white-space: nowrap;
}

.poe-op-row {
  border: 1px solid var(--border-color);
  border-radius: 6px;
  padding: 10px;
  margin-bottom: 12px;
  background-color: var(--bg-secondary);
}

.poe-op-head {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 8px;
}

.poe-op-index {
  font-size: 13px;
  font-weight: 600;
  color: #888;
  white-space: nowrap;
}

.poe-op-desc {
  flex: 1;
}

.poe-op-line {
  display: flex;
  align-items: center;
  gap: 8px;
  width: 100%;
}

.poe-op-line > :not(.poe-keep-origin):not(.n-button) {
  flex: 1;
  min-width: 0;
}

.poe-keep-origin {
  display: flex;
  align-items: center;
  gap: 4px;
  white-space: nowrap;
  flex: 0 0 auto;
}

.poe-keep-origin-label {
  font-size: 12px;
  color: #888;
}

.poe-conditions {
  margin-top: 8px;
  padding-left: 12px;
  display: flex;
  flex-direction: column;
  gap: 8px;
}

.poe-cond-head {
  display: flex;
  align-items: center;
  gap: 8px;
}

.poe-cond-label {
  font-size: 13px;
  color: #888;
  white-space: nowrap;
}

.poe-cond-line {
  display: flex;
  align-items: center;
  gap: 8px;
  width: 100%;
}

.poe-cond-line > :not(.poe-cond-switch):not(.n-button) {
  flex: 1;
  min-width: 0;
}

.poe-cond-switch {
  display: flex;
  align-items: center;
  gap: 4px;
  white-space: nowrap;
  font-size: 12px;
  color: #888;
  flex: 0 0 auto;
}

.poe-cond-hint {
  font-size: 12px;
  color: #999;
  padding-left: 4px;
}

.poe-empty {
  color: #999;
  font-size: 13px;
  padding: 12px 0;
}

.poe-legacy-hint,
.poe-json-hint {
  font-size: 12px;
  color: #999;
  margin-bottom: 6px;
}

.poe-json-error {
  font-size: 12px;
  color: var(--error-color);
  margin-top: 4px;
}

@media (max-width: 768px) {
  .poe-op-line,
  .poe-cond-line {
    flex-direction: column;
    gap: 8px;
    align-items: stretch;
  }

  .poe-op-line > :not(.poe-keep-origin):not(.n-button),
  .poe-cond-line > :not(.poe-cond-switch):not(.n-button) {
    flex: 1;
  }
}
</style>
