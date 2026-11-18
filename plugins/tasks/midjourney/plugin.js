// Midjourney Proxy and Midjourney Proxy Plus native task protocol.
const actions = ["IMAGINE", "DESCRIBE", "BLEND", "UPSCALE", "VARIATION", "REROLL", "INPAINT", "MODAL", "ZOOM", "CUSTOM_ZOOM", "SHORTEN", "HIGH_VARIATION", "LOW_VARIATION", "PAN", "SWAP_FACE", "UPLOAD", "VIDEO", "EDITS"];
const actionLabels = {
  IMAGINE: { en: "Generate images", zh: "生成图片" }, DESCRIBE: { en: "Describe images", zh: "描述图片" },
  BLEND: { en: "Blend images", zh: "混合图片" }, UPSCALE: { en: "Upscale images", zh: "放大图片" },
  VARIATION: { en: "Generate variations", zh: "生成变体" }, REROLL: { en: "Regenerate images", zh: "重新生成图片" },
  INPAINT: { en: "Inpaint images", zh: "局部重绘" }, MODAL: { en: "Submit image dialog", zh: "提交图片对话" },
  ZOOM: { en: "Zoom images", zh: "缩放图片" }, CUSTOM_ZOOM: { en: "Apply custom zoom", zh: "自定义缩放图片" },
  SHORTEN: { en: "Shorten prompts", zh: "缩短提示词" }, HIGH_VARIATION: { en: "Generate strong variations", zh: "生成强变体" },
  LOW_VARIATION: { en: "Generate subtle variations", zh: "生成弱变体" }, PAN: { en: "Extend images", zh: "扩展图片" },
  SWAP_FACE: { en: "Swap faces", zh: "替换人脸" }, UPLOAD: { en: "Upload images", zh: "上传图片" },
  VIDEO: { en: "Generate videos", zh: "生成视频" }, EDITS: { en: "Edit images", zh: "编辑图片" },
};
function saleName(action) { return action === "SWAP_FACE" ? "swap_face" : "mj_" + action.toLowerCase(); }
const routes = [];
for (const prefix of ["/mj", "/fast/mj", "/relax/mj", "/turbo/mj"]) {
  routes.push({ method: "POST", path: prefix + "/submit/:operation", type: "submit", decode: "decodeSubmit", render: "renderSubmit" });
  routes.push({ method: "POST", path: prefix + "/insight-face/swap", type: "submit", decode: "decodeSubmit", render: "renderSubmit" });
  routes.push({ method: "GET", path: prefix + "/task/:id/fetch", type: "query", taskIdParam: "id", render: "renderTask" });
  routes.push({ method: "POST", path: prefix + "/task/list-by-condition", type: "dynamic", decode: "decodeBatch", render: "renderTasks" });
}
export const meta = {
  apiVersion: 1, key: "midjourney", name: "Midjourney", version: "1.0.0", author: { name: "QuantumNous" },
  description: { en: "Midjourney Proxy native tasks", zh: "Midjourney Proxy 原生任务" },
  channelTypes: [2, 5], models: actions.map(saleName), fetchMode: "per_task", upstreams: ["vendor", "new_api"],
  protocols: [{ name: "openai_responses", supports: ["stream", "sync", "background"] }],
  usageSchema: {
    action: { enum: actions, enumLabels: actionLabels, description: { en: "Perform Midjourney action", zh: "执行 Midjourney 动作" } },
    mode: { enum: ["default", "fast", "relax", "turbo"], enumLabels: { default: { en: "Use default mode", zh: "使用默认模式" }, fast: { en: "Use fast mode", zh: "使用快速模式" }, relax: { en: "Use relaxed mode", zh: "使用放松模式" }, turbo: { en: "Use turbo mode", zh: "使用极速模式" } }, description: { en: "Select generation mode", zh: "选择生成模式" } },
    operations: { type: "number", unit: "count", description: { en: "Completed operation unit price", zh: "完成操作单价" } },
    artifacts: { type: "number", unit: "count", description: { en: "Returned artifact unit price", zh: "返回产物单价" } },
    seconds: { type: "number", unit: "second", description: { en: "Generated video duration unit price", zh: "生成视频时长单价" } },
    credits: { type: "number", unit: "credit", description: { en: "Reported Provider credit unit price", zh: "上游报告积分单价" } },
  }, routes: routes,
};
function object(value) { return value !== null && typeof value === "object" && !Array.isArray(value); }
function text(value) { return typeof value === "string" ? value.trim() : ""; }
function bodyOf(ctx) {
  if (!ctx.body || ctx.body.kind !== "json" || !object(ctx.body.value)) throw new Error("JSON object required");
  return ctx.body.value;
}
function modeOf(ctx) {
  const matched = String(ctx.path || "").match(/^\/(?:midjourney\/)?(fast|relax|turbo)\/mj\//);
  const mode = text((ctx.params || {}).mode) || (matched ? matched[1] : "default");
  if (!["default", "fast", "relax", "turbo"].includes(mode)) throw new Error("invalid Midjourney mode");
  return mode;
}
function customAction(customId) {
  const parts = text(customId).split("::");
  if (parts[0] !== "MJ" || parts.length < 2) throw new Error("invalid Midjourney action");
  if (parts[1] === "JOB") parts.splice(1, 1);
  const name = parts[1];
  if (name === "upsample") return "UPSCALE";
  if (name === "variation") return parts[2] === "low" ? "LOW_VARIATION" : parts[2] === "high" ? "HIGH_VARIATION" : "VARIATION";
  const mapping = { reroll: "REROLL", pan: "PAN", Outpaint: "ZOOM", CustomZoom: "CUSTOM_ZOOM", Inpaint: "INPAINT" };
  if (!mapping[name]) throw new Error("invalid Midjourney action");
  return mapping[name];
}
function operationOf(ctx, body) {
  const declared = text((ctx.params || {}).operation);
  if (declared) return declared;
  if (body.customId) return "action";
  if (["UPSCALE", "VARIATION", "REROLL"].includes(ctx.action)) return "change";
  const operation = { IMAGINE: "imagine", DESCRIBE: "describe", BLEND: "blend", SHORTEN: "shorten", MODAL: "modal", EDITS: "edits", VIDEO: "video", UPLOAD: "upload-discord-images", SWAP_FACE: "swap" }[ctx.action];
  if (!operation) throw new Error("Midjourney operation required");
  return operation;
}
function actionOf(ctx, body) {
  if (String(ctx.path || "").endsWith("/insight-face/swap") || ctx.action === "SWAP_FACE") return "SWAP_FACE";
  const operation = operationOf(ctx, body);
  if (operation === "action") return customAction(body.customId);
  if (operation === "simple-change") {
    const match = text(body.content).match(/^(\S+)\s+([uv][1-4]|r)$/i);
    if (!match) throw new Error("invalid simple change");
    return match[2][0].toLowerCase() === "u" ? "UPSCALE" : match[2][0].toLowerCase() === "v" ? "VARIATION" : "REROLL";
  }
  if (operation === "change") {
    const action = text(body.action).toUpperCase();
    if (!["UPSCALE", "VARIATION", "REROLL"].includes(action)) throw new Error("invalid change action");
    return action;
  }
  const mapping = { imagine: "IMAGINE", describe: "DESCRIBE", blend: "BLEND", shorten: "SHORTEN", modal: "MODAL", edits: "EDITS", video: "VIDEO", "upload-discord-images": "UPLOAD" };
  if (!mapping[operation]) throw new Error("invalid Midjourney operation");
  return mapping[operation];
}
function originOf(ctx, body) {
  if ((ctx.params || {}).operation === "simple-change") return text(body.content).split(/\s+/)[0];
  return text(body.taskId);
}
function decodeSubmit(ctx) {
  const body = bodyOf(ctx), action = actionOf(ctx, body), origin = originOf(ctx, body);
  modeOf(ctx);
  if (["UPSCALE", "VARIATION", "REROLL", "INPAINT", "MODAL", "ZOOM", "CUSTOM_ZOOM", "HIGH_VARIATION", "LOW_VARIATION", "PAN"].includes(action) && !origin) throw new Error("origin task required");
  return { kind: "submit", model: saleName(action), action: action, requestBody: body, originTaskIds: origin ? [origin] : [] };
}
function decodeBatch(ctx) {
  const body = bodyOf(ctx);
  if (!Array.isArray(body.ids) || body.ids.length === 0 || body.ids.length > 100 || body.ids.some(id => !text(id))) throw new Error("task ids required");
  return { kind: "query", taskIds: body.ids };
}
function normalized(ctx) {
  const body = Object.assign({}, ctx.requestBody || {});
  const action = text(ctx.action).toUpperCase();
  if (!actions.includes(action) || actionOf(ctx, body) !== action) throw new Error("Midjourney action changed");
  modeOf(ctx);
  if (body.stream === true || body.notifyHook) throw new Error("unsupported Midjourney callback or stream");
  if (["IMAGINE", "SHORTEN"].includes(action) && !text(body.prompt)) throw new Error("prompt required");
  if (["BLEND", "UPLOAD"].includes(action) && (!Array.isArray(body.base64Array) || body.base64Array.length === 0 || body.base64Array.length > 128 || body.base64Array.some(value => !text(value)))) throw new Error("images required");
  if (action === "DESCRIBE" && !text(body.base64)) throw new Error("image required");
  if (body.index !== undefined && (!Number.isInteger(body.index) || body.index < 1 || body.index > 4)) throw new Error("invalid image index");
  return body;
}
export function extractCapabilities(ctx) {
  const body = normalized(ctx), action = ctx.action;
  const input = ["text"];
  if (body.base64 || body.base64Array || body.maskBase64 || body.image || body.imageUrl) input.push("image");
  const required = { input_modalities: input, output_modalities: [action === "VIDEO" ? "video" : ["DESCRIBE", "SHORTEN"].includes(action) ? "text" : "image"] };
  if (action === "UPLOAD") required.image_count = body.base64Array.length;
  return required;
}
export function extractUsage(ctx) { normalized(ctx); return { action: ctx.action, mode: modeOf(ctx), operations: 1 }; }
function prefix(ctx) { const mode = modeOf(ctx); return mode === "default" ? "/mj" : "/" + mode + "/mj"; }
function headers(ctx) { return ctx.upstream && ctx.upstream.kind === "new_api" ? { Authorization: ctx.authHeader } : { "mj-api-secret": ctx.apiKey }; }
export function buildSubmitRequest(ctx) {
  const body = normalized(ctx), origin = originOf(ctx, body);
  if (origin) {
    // decodeSubmit declares exactly one origin. The host has already resolved
    // its owner-scoped public ID or imported legacy alias before this hook.
    const task = (ctx.originTasks || []).length === 1 ? ctx.originTasks[0] : null;
    if (!task || !text(task.upstreamTaskId)) throw new Error("origin task unavailable");
    body.taskId = task.upstreamTaskId;
    if ((ctx.params || {}).operation === "simple-change") body.content = task.upstreamTaskId + " " + text(body.content).split(/\s+/)[1];
    if (body.customId) {
      const buttons = task.data && task.data.buttons;
      const publicButtons = (buttons || []).map((button, index) => ({ id: publicAction(task.taskId, index, button.customId), source: button.customId }));
      const button = publicButtons.find(button => button.id === body.customId);
      if (!button) throw new Error("origin action unavailable");
      body.customId = button.source;
    }
  }
  const operation = ctx.action === "SWAP_FACE" ? "/insight-face/swap" : "/submit/" + operationOf(ctx, body);
  return { url: ctx.baseUrl + prefix(ctx) + operation, method: "POST", headers: headers(ctx), body: body, action: ctx.action };
}
export function parseSubmitResponse(ctx, response) {
  const body = response.body;
  if (!object(body) || !Number.isInteger(body.code)) throw new Error("invalid Midjourney acknowledgement");
  if (body.code === 21) {
    if (!text(body.result)) throw new Error("existing task identity missing");
    return { acceptance: "existing", taskId: body.result, taskData: body };
  }
  if ([2, 3, 4, 23, 24, 25].includes(body.code)) return { acceptance: "rejected", taskId: "", taskData: body };
  if (![1, 22].includes(body.code)) return { acceptance: "unknown", taskId: "" };
  if (ctx.action === "UPLOAD") {
    if (!Array.isArray(body.result) || body.result.length === 0 || body.result.some(url => !/^https?:\/\//.test(url))) throw new Error("invalid uploaded image result");
    return { acceptance: "accepted", delivery: "inline", taskId: ctx.publicTaskId, taskData: { status: "SUCCESS", action: ctx.action, mode: modeOf(ctx), imageUrls: body.result, acknowledgement: body }, immediate: { status: "SUCCESS", progress: "100%" } };
  }
  if (!text(body.result)) throw new Error("Midjourney task identity missing");
  return { acceptance: "accepted", taskId: body.result, taskData: { status: "SUBMITTED", action: ctx.action, mode: modeOf(ctx), acknowledgement: body } };
}
export function buildQueryRequest(ctx) {
  const mode = (ctx.submittedUsage || {}).mode || (ctx.data || {}).mode;
  const route = mode && mode !== "default" ? "/" + mode + "/mj" : "/mj";
  return { url: ctx.baseUrl + route + "/task/" + encodeURIComponent(ctx.taskId) + "/fetch", method: "GET", headers: headers(ctx) };
}
export function parseTaskResult(ctx, body) {
  if (!object(body) || !["NOT_START", "SUBMITTED", "QUEUED", "IN_PROGRESS", "SUCCESS", "FAILURE"].includes(body.status)) throw new Error("invalid Midjourney task result");
  if (body.id && body.id !== ctx.taskId) throw new Error("Midjourney task identity changed");
  return { taskId: ctx.taskId, status: body.status, progress: text(body.progress), reason: text(body.failReason) };
}
function artifactURLs(data) {
  const result = [];
  for (const key of ["imageUrl", "videoUrl"]) if (typeof data[key] === "string" && /^https?:\/\//.test(data[key])) result.push({ type: key === "imageUrl" ? "image" : "video", url: data[key] });
  for (const key of ["imageUrls", "videoUrls"]) if (Array.isArray(data[key])) for (const value of data[key]) {
    const url = typeof value === "string" ? value : object(value) ? value.url : "";
    if (/^https?:\/\//.test(url)) result.push({ type: key === "imageUrls" ? "image" : "video", url: url });
  }
  return result.filter((item, index) => result.findIndex(other => other.url === item.url) === index);
}
export function extractUsageOnComplete(task, result, data) {
  data = data || {};
  const usage = object(data.usage) ? data.usage : {};
  const facts = { action: task.action, mode: (task.submittedUsage || {}).mode || data.mode || "default", operations: result.status === "SUCCESS" ? 1 : null, artifacts: null, seconds: null, credits: null };
  const artifacts = artifactURLs(data);
  if (artifacts.length) facts.artifacts = artifacts.length;
  if (typeof usage.seconds === "number") facts.seconds = usage.seconds;
  if (typeof usage.credits === "number") facts.credits = usage.credits;
  return facts;
}
function publicAction(taskId, index, source) {
  // Retain the action discriminator, never the Provider's opaque action payload.
  const parts = text(source).split("::");
  if (parts[1] === "JOB") parts.splice(1, 1);
  const kind = parts[1], variant = kind === "variation" && ["low", "high"].includes(parts[2]) ? "::" + parts[2] : "";
  return "MJ::" + kind + variant + "::public::" + taskId + "::" + index;
}
function publicTask(ctx, task) {
  const source = task.data || {}, result = {};
  for (const key of ["action", "botType", "prompt", "promptEn", "state", "description", "submitTime", "startTime", "finishTime", "status", "progress", "failReason"]) if (source[key] !== undefined) result[key] = source[key];
  result.id = task.task_id; result.status = task.status; result.progress = task.progress;
  if (Array.isArray(source.buttons)) result.buttons = source.buttons.map((button, index) => ({ customId: publicAction(task.task_id, index, button.customId), label: button.label, emoji: button.emoji, style: button.style, type: button.type }));
  const artifacts = Object.values((ctx.artifactsByTask || {})[task.task_id] || ctx.artifacts || {});
  const images = artifacts.filter(item => item.type === "image" || item.kind === "image");
  const videos = artifacts.filter(item => item.type === "video" || item.kind === "video");
  if (images.length) result.imageUrl = images[0].url;
  if (videos.length) { result.videoUrl = videos[0].url; result.videoUrls = videos.map(item => item.url); }
  if (source.properties && source.properties.seed !== undefined) result.properties = { seed: source.properties.seed };
  return result;
}
function renderSubmit(ctx, task) {
  if ((task.data || {}).action === "UPLOAD") return { code: 1, description: "Success", result: Object.values(ctx.artifacts || {}).map(item => item.url) };
  return { code: 1, description: "Success", result: task.task_id };
}
function renderTask(ctx, task) { return publicTask(ctx, task); }
function renderTasks(ctx, tasks) { return tasks.map(task => publicTask(ctx, task)); }
export function listArtifacts(task) {
  const artifacts = artifactURLs(task.data || {}).map((item, index) => ({ key: index === 0 && item.type === "image" ? "image" : item.type + "-" + index, type: item.type, mimeType: item.type === "image" ? "image/png" : "video/mp4" }));
  if (task.action !== "UPLOAD" && artifacts.some(item => item.type === "image")) artifacts.push({ key: "seed", type: "file", mimeType: "application/json" });
  return artifacts;
}
export function buildContentRequest(ctx) {
  if (ctx.artifactKey === "seed") {
    const mode = (ctx.submittedUsage || {}).mode || (ctx.data || {}).mode;
    const route = mode && mode !== "default" ? "/" + mode + "/mj" : "/mj";
    return { url: ctx.baseUrl + route + "/task/" + encodeURIComponent(ctx.upstreamTaskId) + "/image-seed", method: ctx.clientRequest.method, headers: headers(ctx) };
  }
  const items = artifactURLs(ctx.data || {});
  const index = ctx.artifactKey === "image" ? 0 : Number(String(ctx.artifactKey).split("-").pop());
  if (!Number.isInteger(index) || !items[index]) throw new Error("artifact unavailable");
  return { url: items[index].url, method: ctx.clientRequest.method, credentialless: true };
}
function responseText(ctx, task) {
  const data = task.data || {}, output = [];
  for (const key of ["description", "promptEn"]) if (text(data[key])) output.push(text(data[key]));
  for (const item of Object.values(ctx.artifacts || {})) {
    if (item.type === "image" || item.kind === "image") output.push("![Midjourney image](" + item.url + ")");
    if (item.type === "video" || item.kind === "video") output.push("[Midjourney video](" + item.url + ")");
  }
  return output.join("\n\n") || "Midjourney operation completed.";
}
function decodeResponses(ctx) {
  const request = bodyOf(ctx), declared = text(ctx.upstreamModel || ctx.model);
  const action = actions.find(value => saleName(value) === declared);
  if (!action) throw new Error("unsupported Midjourney model");
  if (request.metadata !== undefined && !object(request.metadata)) throw new Error("metadata must be an object");
  if (request.input !== undefined && typeof request.input !== "string" && !Array.isArray(request.input)) throw new Error("invalid Responses input");
  const body = Object.assign({}, request.metadata || {}), texts = [];
  if (typeof request.input === "string") texts.push(request.input);
  if (Array.isArray(request.input)) for (const item of request.input) {
    if (typeof item === "string") { texts.push(item); continue; }
    if (!object(item)) continue;
    const content = Array.isArray(item.content) ? item.content : [item.content || item];
    for (const part of content) if (object(part) && ["input_text", "text"].includes(part.type) && typeof part.text === "string") texts.push(part.text);
  }
  if (!body.prompt && texts.length) body.prompt = texts.join("\n");
  if (["UPSCALE", "VARIATION", "REROLL"].includes(action) && !body.customId) body.action = action;
  const nativeContext = Object.assign({}, ctx, { action: action, requestBody: body });
  normalized(nativeContext);
  const origin = originOf(nativeContext, body);
  if (["UPSCALE", "VARIATION", "REROLL", "INPAINT", "MODAL", "ZOOM", "CUSTOM_ZOOM", "HIGH_VARIATION", "LOW_VARIATION", "PAN"].includes(action) && !origin) throw new Error("origin task required");
  return { kind: "submit", model: ctx.model, action: action, requestBody: body, originTaskIds: origin ? [origin] : [] };
}
export const protocols = {
  openai_responses: {
    decodeRequest: decodeResponses,
    renderFinal: function (ctx, task) {
      return { output: [{ type: "message", status: "completed", role: "assistant", content: [{ type: "output_text", text: responseText(ctx, task), annotations: [], logprobs: [] }] }], metadata: { vendor: "midjourney" } };
    },
    renderEvents: function (ctx, task, previous) {
      const state = { status: task.status, progress: task.progress };
      if (task.status === "SUCCESS") return { events: previous && previous.status === "SUCCESS" ? [] : [{ type: "output", data: responseText(ctx, task) }], state: state, done: true };
      if (task.status === "FAILURE") return { events: [{ type: "error", code: "task_failed", message: "Midjourney operation failed" }], state: state, done: true };
      if (previous && previous.status === state.status && previous.progress === state.progress) return { events: [], state: state, done: false };
      return { events: [{ type: "progress", message: String(task.status || "UNKNOWN").toLowerCase() }], state: state, done: false };
    },
  },
};
export const native = { decodeSubmit, decodeBatch, renderSubmit, renderTask, renderTasks };
