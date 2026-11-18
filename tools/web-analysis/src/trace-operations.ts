// Trace adapter. The implementations below are the vendored MIT REA web subset.
import { z } from "zod";
import { withClosedResource } from "./trace-resources.js";
import { createBrowserScenarioProvider } from "./composition/browserScenario.js";
import * as browser from "./application/BrowserObservationService.js";
import { captureBrowserScenario } from "./application/BrowserScenarioCaptureService.js";
import { captureElectronScenario } from "./application/javascript/ElectronActiveObservationService.js";
import * as electron from "./application/javascript/ElectronObservationService.js";
import * as runtime from "./application/javascript/JavaScriptRuntimeObservationService.js";
import * as workflows from "./application/javascript/JavaScriptApplicationWorkflowService.js";
import { traceJavaScriptSemanticsEvidenceValidated } from "./application/javascript/JavaScriptSemanticTraceService.js";
import { reconcileJavaScriptRuntimeEvidenceValidated } from "./application/javascript/JavaScriptRuntimeReconciliationService.js";
import { analyzeJavaScriptApplicationValidated } from "./application/javascript/JavaScriptApplicationService.js";
import { exportWebScriptsValidated } from "./application/WebScriptExportService.js";
import { JavaScriptRecoveryService } from "./application/javascript/JavaScriptRecoveryService.js";
import { ArtifactResourceScope } from "./artifacts/ArtifactResourceScope.js";
import { createBrowserObservationProvider } from "./composition/browserObservation.js";
import { createElectronObservationProvider } from "./composition/electronObservation.js";
import { createElectronScenarioProvider } from "./composition/electronScenario.js";
import { createJavaScriptRuntimeObservationProvider } from "./composition/javascriptRuntimeObservation.js";
import { createJavaScriptRecoveryProvider } from "./composition/javascriptRecovery.js";
import { createWebRuntimeService } from "./composition/webRuntime.js";
import { createWebNetworkCaptureService } from "./composition/webNetworkCaptures.js";
import { createWebModuleTraceService } from "./composition/webModules.js";
import { createWebSourceLocationService } from "./composition/webSourceLocations.js";
import { listBrowserTargetsInputSchema, inspectWebPageInputSchema } from "./domain/browserObservation.js";
import { browserScenarioSchema } from "./domain/browserScenario.js";
import { browserCaptureComparisonInputSchema } from "./domain/browserCaptureComparison.js";
import { analyzeWebBundleInputSchema } from "./domain/webBundleAnalysis.js";
import { observeWebSessionInputSchema } from "./domain/browserSession.js";
import { discoverWebMcpToolsInputSchema } from "./domain/webMcpDiscovery.js";
import { captureWebScreenshotInputSchema, compareWebScreenshotsInputSchema } from "./domain/webScreenshot.js";
import { observeWebExecutionInputSchema } from "./domain/webExecution.js";
import { inspectWebEventListenersInputSchema } from "./domain/webEventListeners.js";
import { inspectWebNetworkCaptureInputSchema } from "./domain/webNetworkCapture.js";
import { exportWebScriptsInputSchema } from "./domain/webScriptExport.js";
import { webModuleTraceInputSchema } from "./domain/webModuleTrace.js";
import { webSourceLocationInputSchema } from "./domain/webSourceLocation.js";
import { analyzeJavaScriptApplicationInputSchema } from "./domain/javascript/javascriptApplicationAnalysis.js";
import { traceJavaScriptSemanticsInputSchema } from "./domain/javascript/javascriptSemanticTraceSchemas.js";
import { traceApplicationFeatureInputSchema } from "./domain/javascript/javascriptFeatureTraceSchemas.js";
import { compareApplicationVersionsInputSchema } from "./domain/javascript/javascriptApplicationVersionComparisonSchemas.js";
import { compareJavaScriptExportShapesInputSchema } from "./domain/javascript/javascriptExportShapeComparisonSchemas.js";
import { compareSourceToBundleInputSchema } from "./domain/javascript/sourceToBundleComparisonSchemas.js";
import { reconcileJavaScriptRuntimeInputSchema } from "./domain/javascript/javascriptRuntimeReconciliationSchemas.js";
import { javascriptRecoveryInputSchema } from "./domain/javascript/javascriptRecovery.js";
import { listJavaScriptRuntimeTargetsInputSchema, observeJavaScriptRuntimeInputSchema } from "./domain/javascript/javascriptRuntimeObservation.js";
import { listElectronTargetsInputSchema, inspectElectronPageInputSchema } from "./domain/javascript/electronObservation.js";
import { electronActiveObservationInputSchema } from "./domain/javascript/electronActiveObservation.js";

type Operation = { schema: z.ZodType; description: string; run: (input: any, signal?: AbortSignal) => any };
const options = (signal?: AbortSignal) => signal ? { signal } : {};
const scoped = async (run: (scope: ArtifactResourceScope) => Promise<any>) => {
  const scope = new ArtifactResourceScope();
  return withClosedResource(scope, () => run(scope));
};
const closed = withClosedResource;

export const operations: Record<string, Operation> = {
  analyze_javascript_application: { schema: analyzeJavaScriptApplicationInputSchema, description: "ASAR/目录静态分析：完整 JavaScript 语义图、跨模块应用图、Electron 边界及覆盖限制。", run: (i, s) => scoped(r => analyzeJavaScriptApplicationValidated(i, r, options(s))) },
  trace_javascript_semantics: { schema: traceJavaScriptSemanticsInputSchema, description: "查询语义图中的数据流、调用、返回值、对象属性、异步关系与来源。", run: traceJavaScriptSemanticsEvidenceValidated },
  trace_application_feature: { schema: traceApplicationFeatureInputSchema, description: "从应用图种子追踪跨文件、IPC、preload 与资源依赖。", run: workflows.traceApplicationFeatureEvidenceValidated },
  compare_application_versions: { schema: compareApplicationVersionsInputSchema, description: "比较两个应用版本的结构、关系和功能变化。", run: workflows.compareApplicationVersionsEvidenceValidated },
  compare_javascript_export_shapes: { schema: compareJavaScriptExportShapesInputSchema, description: "比较模块导出结构与返回形状。", run: workflows.compareJavaScriptExportShapesEvidenceValidated },
  compare_source_to_bundle: { schema: compareSourceToBundleInputSchema, description: "比较源代码与打包产物，保留匹配证据和不确定性。", run: workflows.compareSourceToBundleEvidenceValidated },
  reconcile_javascript_runtime: { schema: reconcileJavaScriptRuntimeInputSchema, description: "将真实运行观测与静态应用图关联，区分观测与推断。", run: reconcileJavaScriptRuntimeEvidenceValidated },
  recover_javascript_sources: { schema: javascriptRecoveryInputSchema, description: "通过显式配置的 Wakaru 恢复打包/混淆 JS，保留原始摘要与派生产物。", run: (i,s) => { const service = new JavaScriptRecoveryService(createJavaScriptRecoveryProvider()); return closed(service, () => service.recover(i, options(s))); } },
  list_browser_targets: { schema: listBrowserTargetsInputSchema, description: "列出指定 CDP 端点的网页目标。", run: (i,s) => browser.listBrowserTargets(createBrowserObservationProvider(), i, options(s)) },
  inspect_web_page: { schema: inspectWebPageInputSchema, description: "读取 DOM、脚本、网络、存储及页面元数据。", run: (i,s) => browser.inspectWebPage(createBrowserObservationProvider(), i, options(s)) },
  analyze_web_bundle: { schema: analyzeWebBundleInputSchema, description: "捕获脚本并解析 bundle、路由、接口、框架及 source-map 声明。", run: (i,s) => browser.analyzeWebBundle(createBrowserObservationProvider(), i, options(s)) },
  observe_web_session: { schema: observeWebSessionInputSchema, description: "在有界时间窗记录页面导航、脚本、网络与工作线程。", run: (i,s) => browser.observeWebSession(createBrowserObservationProvider(), i, options(s)) },
  discover_webmcp_tools: { schema: discoverWebMcpToolsInputSchema, description: "发现网页 WebMCP 声明，不自动调用站点工具。", run: (i,s) => browser.discoverWebMcpTools(createBrowserObservationProvider(), i, options(s)) },
  capture_browser_scenario: { schema: browserScenarioSchema, description: "CloakBrowser 有头执行浏览器操作序列，采集前后 DOM、网络、存储与截图；也可连接指定 CDP 目标。", run: (i,s) => { const provider = createBrowserScenarioProvider(process.env); return closed(provider, () => captureBrowserScenario(provider, i, options(s))); } },
  compare_web_captures: { schema: browserCaptureComparisonInputSchema, description: "比较被动页面捕获或浏览器场景，保留缺失覆盖和未知项。", run: i => browser.compareWebCaptureEvidence(createBrowserObservationProvider(), i) },
  capture_web_screenshot: { schema: captureWebScreenshotInputSchema, description: "捕获 CDP 页面 PNG 截图及来源。", run: (i,s) => browser.captureWebScreenshot(createBrowserObservationProvider(), i, options(s)) },
  compare_web_screenshots: { schema: compareWebScreenshotsInputSchema, description: "对两个 PNG 截图计算视觉差异。", run: i => browser.compareWebScreenshotEvidence(createBrowserObservationProvider(), i) },
  export_web_scripts: { schema: exportWebScriptsInputSchema, description: "将捕获中的脚本及 source-map 导出成摘要绑定的文件清单。", run: (i,s) => scoped(r => exportWebScriptsValidated(i, r, options(s))) },
  trace_web_module_imports: { schema: webModuleTraceInputSchema, description: "验证导出脚本摘要并用 CloakBrowser 原生规则解析 ESM/import-map。", run: (i,s) => { const service=createWebModuleTraceService(); return closed(service, () => service.trace(i, options(s))); } },
  trace_web_source_location: { schema: webSourceLocationInputSchema, description: "验证脚本与 source-map 摘要，将生成位置映射到原始来源。", run: (i,s) => { const service=createWebSourceLocationService(); return closed(service, () => service.trace(i, options(s))); } },
  observe_web_execution: { schema: observeWebExecutionInputSchema, description: "CDP 精确执行覆盖与请求发起位置归因。", run: (i,s) => createWebRuntimeService().observe(i, options(s)) },
  inspect_web_event_listeners: { schema: inspectWebEventListenersInputSchema, description: "检查指定节点事件监听器及脚本位置。", run: (i,s) => createWebRuntimeService().inspect(i, options(s)) },
  inspect_web_network_capture: { schema: inspectWebNetworkCaptureInputSchema, description: "离线解析 HAR、SAZ、mitmproxy，保留原记录顺序、正文、元数据与限制。", run: (i,s) => createWebNetworkCaptureService().inspect(i, options(s)) },
  list_javascript_runtime_targets: { schema: listJavaScriptRuntimeTargetsInputSchema, description: "列出 Node/Electron V8 Inspector 目标。", run: (i,s) => runtime.listJavaScriptRuntimeTargets(createJavaScriptRuntimeObservationProvider(), i, options(s)) },
  observe_javascript_runtime: { schema: observeJavaScriptRuntimeInputSchema, description: "附加 V8 Inspector 观察脚本、上下文、模块与加载状态。", run: (i,s) => runtime.observeJavaScriptRuntime(createJavaScriptRuntimeObservationProvider(), i, options(s)) },
  list_electron_targets: { schema: listElectronTargetsInputSchema, description: "列出指定 CDP 端点中的 Electron 本地文件页面及其路径，供后续检查选择目标。", run: (i,s) => electron.listElectronTargets(createElectronObservationProvider(), i, options(s)) },
  inspect_electron_page: { schema: inspectElectronPageInputSchema, description: "被动观测 Electron 页面、脚本及上下文隔离边界。", run: (i,s) => electron.inspectElectronPage(createElectronObservationProvider(), i, options(s)) },
  capture_electron_scenario: { schema: electronActiveObservationInputSchema, description: "启动显式选择的 Electron 程序，记录窗口、IPC、contextBridge 和操作时序，清理所属进程。", run: (i,s) => captureElectronScenario(createElectronScenarioProvider(process.env), i, options(s)) },
};

export function catalog() {
  return Object.entries(operations).map(([name, operation]) => ({
    name, description: operation.description,
    input_schema: z.toJSONSchema(operation.schema, { io: "input", unrepresentable: "any" }),
  }));
}

export async function execute(operation: string, input: unknown, signal?: AbortSignal) {
  signal?.throwIfAborted();
  const selected = operations[operation];
  if (!selected) throw new Error(`Unknown web operation: ${operation}`);
  return await selected.run(selected.schema.parse(input), signal);
}
