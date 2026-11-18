"""Source contract checks; run only in the designated server checkout."""
import unittest
from unittest.mock import patch
from browser_action import set_document


class Page:
    url = "https://fixture.invalid/path"

    def __init__(self, failure=None, navigate=False):
        self.writes = []
        self.waits = []
        self.failure = failure
        self.navigate = navigate
        self.disposed = False

    def set_content(self, html):
        raise TimeoutError("native timeout")

    def evaluate(self, code, value):
        self.writes.append((code, value))
        if self.navigate:
            self.url = "https://changed.invalid/"

    def wait_for_function(self, code, **options):
        self.waits.append((code, options))
        if self.failure:
            raise self.failure
        return self

    def dispose(self):
        self.disposed = True


class DocumentContract(unittest.TestCase):
    def test_one_write_literal_url_wait_and_disposal(self):
        page = Page()
        html = "<title>中文</title><p>');throw new Error('not code')</p>"
        result = set_document(page, html, ready_selector="#done", timeout=1234)
        self.assertEqual(len(page.writes), 1)
        self.assertEqual(page.writes[0][1], html)
        self.assertNotIn(html, page.writes[0][0])
        self.assertEqual(page.waits[0][1]["arg"], [page.url, "load", "#done"])
        self.assertGreater(page.waits[0][1]["timeout"], 0)
        self.assertLessEqual(page.waits[0][1]["timeout"], 1234)
        self.assertEqual(result["url"], page.url)
        self.assertTrue(page.disposed)
        with self.assertRaisesRegex(TimeoutError, "native timeout"):
            page.set_content(html)
        self.assertEqual(len(page.writes), 1, "native method is never patched or retried")

    def test_errors_timeout_and_url_change_propagate_without_rewrite(self):
        for failure in [TimeoutError("wait timed out"), RuntimeError("invalid selector")]:
            page = Page(failure)
            with self.assertRaises(type(failure)):
                set_document(page, "<p>once</p>")
            self.assertEqual(len(page.writes), 1)
        page = Page(navigate=True)
        with self.assertRaisesRegex(RuntimeError, "URL changed"):
            set_document(page, "<p>once</p>")
        self.assertEqual(len(page.writes), 1)
        page = Page()
        with patch("browser_action.time.monotonic", side_effect=[0, 2]):
            with self.assertRaisesRegex(TimeoutError, "effects may already exist"):
                set_document(page, "<p>once</p>", timeout=1000)
        self.assertEqual(len(page.writes), 1)

    def test_invalid_input_never_writes(self):
        for options in [{"html": None}, {"wait_until": "commit"}, {"timeout": 0}, {"timeout": True}, {"timeout": float('nan')}, {"timeout": 30001}, {"ready_selector": " "}]:
            page = Page()
            args = {"html": "<p>ok</p>", **options}
            with self.assertRaises(ValueError):
                set_document(page, **args)
            self.assertEqual(page.writes, [])


if __name__ == "__main__":
    unittest.main()
