// JSON as the server wrote it, re-indented without ever becoming JS values.
// JSON.parse turns every number into a float64, so 9007199254740993 would
// show as 9007199254740992 and 1e400 as null, while the server keeps the
// agent's numbers exactly: the person would approve one value and the
// receiver be sent another. Here each token is copied through as written,
// and JSON.parse is used only to check that the text is JSON at all.

function isJson(text: string) {
  try {
    JSON.parse(text);
    return true;
  } catch {
    return false;
  }
}

function isSpace(c: string | undefined) {
  return c === " " || c === "\t" || c === "\n" || c === "\r";
}

/** Valid JSON text laid out with the given indent; "" gives it on one line with no spaces. */
function layout(text: string, indent: string) {
  let out = "";
  let depth = 0;
  const newline = () => (indent ? "\n" + indent.repeat(depth) : "");
  for (let i = 0; i < text.length; i++) {
    const c = text[i];
    if (c === '"') {
      let end = i + 1;
      while (text[end] !== '"') end += text[end] === "\\" ? 2 : 1;
      out += text.slice(i, end + 1);
      i = end;
    } else if (c === "{" || c === "[") {
      const close = c === "{" ? "}" : "]";
      let next = i + 1;
      while (isSpace(text[next])) next++;
      if (text[next] === close) {
        out += c + close;
        i = next;
      } else {
        depth++;
        out += c + newline();
      }
    } else if (c === "}" || c === "]") {
      depth--;
      out += newline() + c;
    } else if (c === ",") {
      out += "," + newline();
    } else if (c === ":") {
      out += indent ? ": " : ":";
    } else if (!isSpace(c)) {
      out += c;
    }
  }
  return out;
}

/** Pretty JSON with every number and string exactly as written, or the text as is when it is not JSON. */
export function prettyJson(text: string) {
  return isJson(text) ? layout(text, "  ") : text;
}

/** Whether two JSON texts are the same JSON written with different whitespace; numbers are compared as written. */
export function sameJson(a: string, b: string) {
  return isJson(a) && isJson(b) && layout(a, "") === layout(b, "");
}
