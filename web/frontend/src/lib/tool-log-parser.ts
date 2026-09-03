// Matches ANSI SGR colour escapes so they can be stripped from tool output.
// no-control-regex is disabled deliberately: \u001B is the escape character
// this pattern exists to find, so the rule has nothing useful to say here.
// eslint-disable-next-line no-control-regex
const ANSI_PATTERN = /\u001B\[([0-9;]*)m/g

export type ToolCallEntry = {
  kind: "call"
  time: string
  tool: string
  argsPreview: string
  rawLine: string
}

export type ToolResultEntry = {
  kind: "result"
  time: string
  tool: string
  resultPreview: string
  resultFull: string
  hasError: boolean
  rawLine: string
}

export type ToolLogEntry = ToolCallEntry | ToolResultEntry

function stripAnsi(s: string): string {
  return s.replace(ANSI_PATTERN, "")
}

function decodeBase64Utf8(b64: string): string {
  const binary = atob(b64)
  const bytes = new Uint8Array(binary.length)

  for (let i = 0; i < binary.length; i += 1) {
    bytes[i] = binary.charCodeAt(i)
  }

  return new TextDecoder().decode(bytes)
}

function parseFields(line: string): Record<string, string> {
  const fields: Record<string, string> = {}
  const re = /(\w+)=(?:"([^"]*)"|([\S]+))/g
  let m
  while ((m = re.exec(line)) !== null) {
    fields[m[1]] = m[2] ?? m[3]
  }
  return fields
}

export function parseToolLogs(lines: string[]): ToolLogEntry[] {
  const entries: ToolLogEntry[] = []
  for (const rawLine of lines) {
    const line = stripAnsi(rawLine)

    // Match "Tool call: name(args)"
    const callMatch = line.match(/Tool call: (\w+)\((.*)/)
    if (callMatch) {
      const tool = callMatch[1]
      const argsRaw = callMatch[2]
      const argsPreview = argsRaw.endsWith(")") ? argsRaw.slice(0, -1) : argsRaw
      const timeMatch = line.match(/^(\d{2}:\d{2}:\d{2})/)
      entries.push({ kind: "call", time: timeMatch?.[1] ?? "", tool, argsPreview, rawLine })
      continue
    }

    // Match "Tool result: name"
    const resultMatch = line.match(/Tool result: (\w+)/)
    if (resultMatch) {
      const tool = resultMatch[1]
      const fields = parseFields(line)
      const timeMatch = line.match(/^(\d{2}:\d{2}:\d{2})/)
      const b64 = fields["result_b64"] ?? ""
      let resultFull = ""
      if (b64) {
        try {
          resultFull = decodeBase64Utf8(b64)
        } catch {
          // fall through to preview
        }
      }
      entries.push({
        kind: "result",
        time: timeMatch?.[1] ?? "",
        tool,
        resultPreview: fields["result_preview"] ?? "",
        resultFull: resultFull || fields["result_preview"] || "",
        hasError: fields["has_error"] === "true",
        rawLine,
      })
    }
  }
  return entries
}
