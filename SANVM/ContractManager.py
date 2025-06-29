from VM import SANVirtualMachine
from Storage import Storage
from OpCode import OpCode

class ContractManager:
    def __init__(self, storage=None):
        self.storage = storage if storage else Storage()

        if not hasattr(self.storage, "contracts"):
            if "contracts" not in self.storage.data:
                self.storage.data["contracts"] = {}
            self.contracts = self.storage.data["contracts"]
        else:
            self.contracts = self.storage.contracts

    @staticmethod
    def is_256bit_or_smaller_str(value: str) -> bool:
        # 2^256 = 115792089237316195423570985008687907853269984665640564039457584007913129639936
        max_256_str = "115792089237316195423570985008687907853269984665640564039457584007913129639936"

        trimmed_value = value.lstrip('0')

        if not trimmed_value:
            return True

        if len(trimmed_value) < len(max_256_str):
            return True
        elif len(trimmed_value) > len(max_256_str):
            return False
        else:
            return trimmed_value < max_256_str

    def deploy_contract(self, contract_id, bytecode):
        is_valid = self.is_256bit_or_smaller_str(contract_id)

        if (contract_id in self.contracts) and is_valid:
            raise ValueError("This contract id already exists")

        self.contracts[contract_id] = {
            "bytecode": bytecode,
            "storage": {}
        }

    def call_contract_function(self, contract_id, function_name, args):
        if contract_id not in self.contracts:
            raise ValueError(f"{contract_id} is not a valid contract")

        contract_info = self.contracts[contract_id]
        vm = SANVirtualMachine(storage=self._dict_to_storage(contract_info["storage"]))

        temporary_bytecode = list(contract_info["bytecode"])

        temporary_bytecode.append(OpCode.PUSH.value)
        temporary_bytecode.append(function_name)

        param_count = len(args)
        temporary_bytecode.append(OpCode.PUSH.value)
        temporary_bytecode.append(param_count)

        for arg in reversed(args):
            temporary_bytecode.append(OpCode.PUSH.value)
            temporary_bytecode.append(arg)

        temporary_bytecode.append(OpCode.CALL_FUNC.value)

        vm.run(temporary_bytecode)

        return_value = None
        if vm.stack:
            return_value = vm.stack[-1]

        updated_storage = self._storage_to_dict(vm.storage)
        self.contracts[contract_id]["storage"] = updated_storage

        return return_value

    def _dict_to_storage(self, data_dict):
        storage = self.storage
        for key, val in data_dict.items():
            storage.set_var(key, val)
        return storage

    @staticmethod
    def _storage_to_dict(storage_obj):
        return dict(storage_obj.data)
