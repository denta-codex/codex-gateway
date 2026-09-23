"""Decode ChatGPT v1 message snapshots and patches without exposing reasoning text."""
class TextStream:
    def __init__(self):
        self.final = False
        self.message_id = None
        self.text = ''
        self.path = ''
        self.operation = 'add'

    def consume(self, event):
        value = event.get('v')
        if event.get('o') == 'patch' and isinstance(value, list):
            for patch in value:
                if isinstance(patch, dict):
                    yield from self.consume(patch)
            return
        message = value.get('message') if isinstance(value, dict) else event.get('message')
        if isinstance(message, dict):
            identity = message.get('id')
            if identity != self.message_id:
                self.text = ''
                self.message_id = identity
            self.final = (message.get('author', {}).get('role') == 'assistant'
                          and message.get('content', {}).get('content_type') == 'text'
                          and message.get('channel') in (None, 'final'))
            self.path, self.operation = '', 'add'
            if self.final:
                parts = message.get('content', {}).get('parts', [])
                snapshot = ''.join(p for p in parts if isinstance(p, str))
                yield from self.snapshot(snapshot)
            return
        self.path = event.get('p', self.path)
        self.operation = event.get('o', self.operation)
        if not self.final or self.path != '/message/content/parts/0' or not isinstance(value, str):
            return
        if self.operation == 'append':
            self.text += value
            if value:
                yield value
        elif self.operation in ('replace', 'add'):
            yield from self.snapshot(value)

    def snapshot(self, text):
        if not text.startswith(self.text):
            raise ValueError('non_append_text_revision')
        delta = text[len(self.text):]
        self.text = text
        if delta:
            yield delta
