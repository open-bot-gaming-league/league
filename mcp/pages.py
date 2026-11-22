"""The one HTML page shell every server-rendered page uses (landing,
docs index, sign-in). Callers escape their own content."""

PAGE = """<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{title}</title>
<style>body{{font:16px/1.5 system-ui,sans-serif;max-width:40rem;margin:2rem auto;padding:0 1rem}}{style}</style>
</head><body>
{body}
</body></html>
"""


SMALL_STYLE = ".small{font-size:.875rem;color:#555}"

# The legal pages (served by server.py from docs/PRIVACY.md, docs/TERMS.md):
# (docs() name, URL path, link text).
LEGAL_PAGES = (
    ("privacy", "/docs/privacy", "Privacy Policy"),
    ("terms", "/docs/terms", "Terms of Service"),
)


def legal_link(name: str, attrs: str = "") -> str:
    """The link to one legal page by docs() name. attrs goes inside the <a>
    tag (e.g. to open in a new tab)."""
    _, path, label = next(page for page in LEGAL_PAGES if page[0] == name)
    return f'<a href="{path}"{attrs}>{label}</a>'


def legal_links(exclude: str | None = None) -> str:
    """Links to the legal pages, `exclude` (a docs() name) left out."""
    return " &middot; ".join(legal_link(name) for name, _, _ in LEGAL_PAGES if name != exclude)


def render(title: str, body: str, style: str = "") -> str:
    return PAGE.format(title=title, body=body, style=style)
