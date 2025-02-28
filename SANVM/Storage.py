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