import { Type } from "typebox";
import type { ExtensionAPI } from "@earendil-works/pi-coding-agent";

// Web search and fetch for pi through dspool's /v1/tools/* proxy.
//
// dspool writes this extension into pi's extensions directory on `dspool pi`
// and exports DSPOOL_API_KEY and DSPOOL_BASE_URL for that session. The tools
// stay inactive in plain pi sessions, which do not have those variables.

const BASE_URL = (process.env.DSPOOL_BASE_URL || "http://127.0.0.1:8787").replace(/\/+$/, "");
const API_KEY = process.env.DSPOOL_API_KEY || "";
const ENABLED = Boolean(process.env.DSPOOL_BASE_URL || process.env.DSPOOL_API_KEY);
const MAX_OUTPUT_CHARS = 24000;
const MAX_RESULT_CONTENT_CHARS = 4000;

interface SearchResult {
	title?: string;
	url?: string;
	snippet?: string;
	summary?: string;
	content?: string;
	site_name?: string | null;
	date?: string | null;
	authority?: number;
	rerank_score?: number;
}

interface FetchResult {
	title?: string;
	content?: string;
}

function clip(text: string, max: number): string {
	if (text.length <= max) return text;
	return `${text.slice(0, max)}\n…[truncated ${text.length - max} characters]`;
}

async function postJSON(path: string, payload: unknown, signal?: AbortSignal): Promise<any> {
	const headers: Record<string, string> = { "Content-Type": "application/json" };
	if (API_KEY) headers.Authorization = `Bearer ${API_KEY}`;

	let response: Response;
	try {
		response = await fetch(`${BASE_URL}${path}`, {
			method: "POST",
			headers,
			body: JSON.stringify(payload),
			signal,
		});
	} catch (error) {
		const reason = error instanceof Error ? error.message : String(error);
		throw new Error(`Could not reach dspool at ${BASE_URL}: ${reason}. Run \`dspool start\` (or \`dspool pi\`) and try again.`);
	}

	const text = await response.text();
	if (!response.ok) {
		let message = clip(text, 500);
		try {
			const parsed = JSON.parse(text);
			message = parsed?.error?.message || parsed?.message || message;
		} catch {
			// Not JSON; keep the raw snippet.
		}
		const hint = response.status === 401 && !API_KEY ? " DSPOOL_API_KEY is not set; run `dspool pi`." : "";
		throw new Error(`dspool ${path} failed (HTTP ${response.status}): ${message}${hint}`);
	}

	try {
		return JSON.parse(text);
	} catch {
		throw new Error(`dspool ${path} returned invalid JSON`);
	}
}

export default function dspoolTools(pi: ExtensionAPI) {
	if (!ENABLED) return;

	pi.registerTool({
		name: "web_search",
		label: "Web Search",
		description:
			"Search the web for current information through dspool's DeepSeek search API. Returns titles, URLs, and summaries.",
		promptSnippet: "Search the web for current information",
		promptGuidelines: [
			"Use web_search when the answer depends on current or external information; cite the returned URLs.",
		],
		parameters: Type.Object({
			query: Type.String({ description: "Search query (1-128 characters)" }),
			count: Type.Optional(
				Type.Integer({ minimum: 1, maximum: 50, description: "Number of results, 1-50 (default 10)" }),
			),
			need_content: Type.Optional(
				Type.Boolean({ description: "Include extracted page content when available" }),
			),
			ori_question: Type.Optional(
				Type.String({ description: "The original user question, for search context" }),
			),
		}),
		async execute(_toolCallId, params, signal) {
			const payload: Record<string, unknown> = { query: params.query };
			if (params.count !== undefined) payload.count = params.count;
			if (params.need_content !== undefined) payload.need_content = params.need_content;
			if (params.ori_question) payload.ori_question = params.ori_question;

			const data = await postJSON("/v1/tools/search", payload, signal);
			const results: SearchResult[] = Array.isArray(data?.results) ? data.results : [];

			const rendered = results.length
				? results
						.map((result, index) => {
							const lines = [`${index + 1}. ${result.title || "(untitled)"}`, `URL: ${result.url || ""}`];
							if (result.site_name) lines.push(`Site: ${result.site_name}`);
							if (result.date) lines.push(`Date: ${result.date}`);
							const summary = result.summary || result.snippet;
							if (summary) lines.push(summary);
							if (params.need_content && result.content) {
								lines.push(`Content:\n${clip(result.content, MAX_RESULT_CONTENT_CHARS)}`);
							}
							return lines.join("\n");
						})
						.join("\n\n")
				: "No results found.";

			const stored = results.map((result) => ({
				...result,
				content: result.content ? clip(result.content, MAX_RESULT_CONTENT_CHARS) : result.content,
			}));
			return {
				content: [{ type: "text" as const, text: clip(rendered, MAX_OUTPUT_CHARS) }],
				details: { results: stored },
			};
		},
	});

	pi.registerTool({
		name: "web_fetch",
		label: "Web Fetch",
		description:
			"Fetch a web page and return its content as Markdown through dspool's DeepSeek fetch API.",
		promptSnippet: "Fetch a web page as Markdown",
		promptGuidelines: ["Use web_fetch to read a specific URL returned by web_search."],
		parameters: Type.Object({
			url: Type.String({ description: "HTTP or HTTPS URL to fetch" }),
		}),
		async execute(_toolCallId, params, signal) {
			const data: FetchResult = await postJSON("/v1/tools/fetch", { url: params.url }, signal);
			const title = typeof data?.title === "string" ? data.title : "";
			const content = typeof data?.content === "string" ? data.content : "";
			const text = [`Title: ${title || "(untitled)"}`, "", "Content:", clip(content, MAX_OUTPUT_CHARS)].join("\n");

			return {
				content: [{ type: "text" as const, text }],
				details: { title, content: clip(content, MAX_OUTPUT_CHARS) },
			};
		},
	});
}
