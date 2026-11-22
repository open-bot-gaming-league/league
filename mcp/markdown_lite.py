"""A small markdown-to-HTML renderer for the legal pages (/docs/privacy,
/docs/terms), so the server needs no markdown package.

It handles exactly what those two documents use: `#`..`###` headings,
paragraphs, `- ` bullet lists, **bold**, *italic*, `code` and
[text](url) links. Anything else a document might grow (numbered lists,
tables, quotes, fenced code) raises ValueError instead of rendering
wrong: add support here, with a test, when a document needs it.
"""

import html
import re

_UNSUPPORTED = re.compile(r"^(```|>|\||\d+[.)] |\s+\S)")
_HEADING = re.compile(r"^(#{1,3}) (.+)$")
_CODE = re.compile(r"`([^`]+)`")
_LINK = re.compile(r"\[([^\]]+)\]\(([^)\s]+)\)")
_BOLD = re.compile(r"\*\*(.+?)\*\*")
_ITALIC = re.compile(r"\*(.+?)\*")
_SAFE_HREF = re.compile(r"^(https?://|mailto:|/|#)")


def _inline(text: str) -> str:
    text = html.escape(text)
    codes: list[str] = []

    def stash(m: re.Match) -> str:
        codes.append(f"<code>{m.group(1)}</code>")
        return f"\0{len(codes) - 1}\0"

    text = _CODE.sub(stash, text)  # code spans first: nothing inside them is markup
    text = _LINK.sub(lambda m: f'<a href="{m.group(2)}">{m.group(1)}</a>' if _SAFE_HREF.match(m.group(2)) else m.group(0), text)
    text = _BOLD.sub(r"<strong>\1</strong>", text)
    text = _ITALIC.sub(r"<em>\1</em>", text)
    return re.sub(r"\0(\d+)\0", lambda m: codes[int(m.group(1))], text)


def render_markdown(text: str) -> tuple[str, str]:
    """(title, body HTML): the title is the first heading's text."""
    out: list[str] = []
    title = ""
    for block in re.split(r"\n\s*\n", text.strip()):
        lines = block.split("\n")
        for line in lines:
            if _UNSUPPORTED.match(line):
                raise ValueError(f"markdown not supported by the legal-page renderer: {line!r}")
        heading = _HEADING.match(lines[0])
        if heading:
            if len(lines) > 1:
                raise ValueError(f"heading must stand alone: {lines[0]!r}")
            level = len(heading.group(1))
            title = title or re.sub(r"[*`]", "", heading.group(2))
            out.append(f"<h{level}>{_inline(heading.group(2))}</h{level}>")
        elif all(line.startswith("- ") for line in lines):
            out.append("<ul>\n" + "\n".join(f"<li>{_inline(line[2:])}</li>" for line in lines) + "\n</ul>")
        else:
            out.append(f"<p>{_inline(' '.join(lines))}</p>")
    return title, "\n".join(out)
