from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from typing import ClassVar as _ClassVar, Optional as _Optional

DESCRIPTOR: _descriptor.FileDescriptor

class Empty(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class Envelope(_message.Message):
    __slots__ = ("payload",)
    PAYLOAD_FIELD_NUMBER: _ClassVar[int]
    payload: bytes
    def __init__(self, payload: _Optional[bytes] = ...) -> None: ...

class SyncRequest(_message.Message):
    __slots__ = ("from_index", "limit")
    FROM_INDEX_FIELD_NUMBER: _ClassVar[int]
    LIMIT_FIELD_NUMBER: _ClassVar[int]
    from_index: int
    limit: int
    def __init__(self, from_index: _Optional[int] = ..., limit: _Optional[int] = ...) -> None: ...

class SyncResponse(_message.Message):
    __slots__ = ("payload",)
    PAYLOAD_FIELD_NUMBER: _ClassVar[int]
    payload: bytes
    def __init__(self, payload: _Optional[bytes] = ...) -> None: ...
