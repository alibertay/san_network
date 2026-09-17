import copy


class Storage:
    def __init__(self):
        self.data = {}
        self.functions = {}
        self.contracts = {}

    def set_var(self, key, value):
        self.data[key] = value

    def get_var(self, key):
        return self.data.get(key, 0)

    def delete_var(self, key):
        if key in self.data:
            del self.data[key]

    def has_var(self, key):
        return key in self.data

    def to_dict(self):
        """Deep, JSON-serializable snapshot of the storage.

        Everything is copied so callers can mutate their snapshot without
        touching live state.
        """
        return {
            "data": copy.deepcopy(self.data),
            "functions": copy.deepcopy(self.functions),
            "contracts": copy.deepcopy(self.contracts),
        }

    @classmethod
    def from_dict(cls, payload):
        storage = cls()
        storage.load_from_dict(payload)
        return storage

    def load_from_dict(self, payload):
        """Replace the contents (deep copy) without replacing the object.

        The VM keeps a reference to this Storage instance, so mutating in place
        keeps ``node.storage`` and ``node.vm.storage`` in sync.
        """
        payload = payload or {}
        self.data = copy.deepcopy(payload.get("data", {}) or {})
        self.functions = copy.deepcopy(payload.get("functions", {}) or {})
        self.contracts = copy.deepcopy(payload.get("contracts", {}) or {})
