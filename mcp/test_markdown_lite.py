"""markdown_lite.py: the renderer behind /docs/privacy and /docs/terms.
Pure functions; no server. Run: python -m pytest mcp/test_markdown_lite.py -q
"""

import os
import sys

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))

import pytest

from markdown_lite import render_markdown


def test_headings_paragraphs_and_the_title():
    title, body = render_markdown("# The *Title*\n\n## 1. Part\n\nOne\ntwo.\n\nThree.")
    assert title == "The Title"
    assert body == "<h1>The <em>Title</em></h1>\n<h2>1. Part</h2>\n<p>One two.</p>\n<p>Three.</p>"


def test_bullet_list():
    _, body = render_markdown("- **a:** x\n- b")
    assert body == "<ul>\n<li><strong>a:</strong> x</li>\n<li>b</li>\n</ul>"


def test_inline_code_links_and_escaping():
    _, body = render_markdown('A `<b> *x*` and [site](https://e.com/a?b=1&c=2) & "q".')
    assert "<code>&lt;b&gt; *x*</code>" in body  # nothing inside a code span is markup
    assert '<a href="https://e.com/a?b=1&amp;c=2">site</a>' in body
    assert "&amp; &quot;q&quot;." in body


def test_markup_in_text_cannot_inject_html():
    _, body = render_markdown("<script>alert(1)</script> [x](javascript:alert(1))")
    assert "<script>" not in body and "<a " not in body


@pytest.mark.parametrize("text", ["1. one", "> quote", "```\ncode\n```", "| a | b |", "- a\n  - nested", "# h\ntext"])
def test_unsupported_markdown_is_refused_not_misrendered(text):
    with pytest.raises(ValueError):
        render_markdown(text)
