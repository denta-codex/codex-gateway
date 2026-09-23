"""Focused fixtures for ChatGPT text snapshot and patch decoding."""
import unittest

from stream import TextStream


class TextStreamTest(unittest.TestCase):
    def test_final_text_and_patch_array(self):
        stream = TextStream()
        self.assertEqual(list(stream.consume({
            'v': {'message': {'id': 'm1', 'author': {'role': 'assistant'},
                              'content': {'content_type': 'text', 'parts': ['Hel']},
                              'channel': 'final'}}
        })), ['Hel'])
        self.assertEqual(list(stream.consume({'o': 'patch', 'v': [
            {'o': 'append', 'p': '/message/content/parts/0', 'v': 'lo'},
            {'o': 'append', 'p': '/message/content/parts/0', 'v': ' world'},
        ]})), ['lo', ' world'])

    def test_reasoning_is_not_emitted(self):
        stream = TextStream()
        self.assertEqual(list(stream.consume({
            'v': {'message': {'id': 'm1', 'author': {'role': 'assistant'},
                              'content': {'content_type': 'text', 'parts': ['hidden']},
                              'channel': 'analysis'}}
        })), [])
        self.assertEqual(list(stream.consume({'o': 'append', 'p': '/message/content/parts/0', 'v': ' secret'})), [])

    def test_revision_cannot_retract_text(self):
        stream = TextStream()
        list(stream.consume({'v': {'message': {'id': 'm1', 'author': {'role': 'assistant'},
                                              'content': {'content_type': 'text', 'parts': ['hello']},
                                              'channel': 'final'}}}))
        with self.assertRaisesRegex(ValueError, 'non_append_text_revision'):
            list(stream.consume({'o': 'replace', 'p': '/message/content/parts/0', 'v': 'goodbye'}))


if __name__ == '__main__':
    unittest.main()
