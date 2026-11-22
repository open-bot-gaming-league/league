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


def render(title: str, body: str, style: str = "") -> str:
    return PAGE.format(title=title, body=body, style=style)
