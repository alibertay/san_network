import logging

from SANVM.OpCode import OpCode
from SANVM.Storage import Storage

logger = logging.getLogger(__name__)


class ContractManager:
    def __init__(self, storage=None):
        self.storage = storage if storage else Storage()
        self.contracts = self.storage.contracts
        self.last_gas_used = 0
        self.last_logs: list[dict] = []

    @staticmethod
    def is_256bit_or_smaller_str(value: str) -> bool:
        # 2^256 = 115792089237316195423570985008687907853269984665640564039457584007913129639936
        max_256_str = "115792089237316195423570985008687907853269984665640564039457584007913129639936"

        trimmed_value = value.lstrip("0")

        if not trimmed_value:
            return True

        if len(trimmed_value) < len(max_256_str):
            return True
        elif len(trimmed_value) > len(max_256_str):
            return False
        else:
            return trimmed_value < max_256_str

    def deploy_contract(self, contract_id, bytecode, gas_limit=None):
        # Lazy import: SANVM.VM imports this module at import time.
        from SANVM.VM import SANVirtualMachine

        if not isinstance(contract_id, str) or not contract_id:
            raise ValueError(f"Invalid contract id: {contract_id!r}")

        if not self.is_256bit_or_smaller_str(contract_id):
            raise ValueError(f"Invalid contract id: {contract_id!r}")

        if contract_id in self.contracts:
            raise ValueError(f"Contract {contract_id!r} already exists")

        contract_storage = Storage()
        vm = SANVirtualMachine(storage=contract_storage, gas_limit=gas_limit)
        # Runs the contract's top-level code and registers its functions.
        vm.run(list(bytecode))
        self.last_gas_used = vm.gas_used
        self.last_logs = list(vm.logs)

        self.contracts[contract_id] = {
            "bytecode": list(bytecode),
            "storage": contract_storage.to_dict(),
        }
        logger.info("Contract deployed: %s (gas used: %d)", contract_id, vm.gas_used)

    def call_contract_function(self, contract_id, function_name, args, gas_limit=None):
        # Lazy import: SANVM.VM imports this module at import time.
        from SANVM.VM import SANVirtualMachine

        if contract_id not in self.contracts:
            raise ValueError(f"{contract_id} is not a valid contract")

        contract = self.contracts[contract_id]
        contract_bytecode = list(contract["bytecode"])

        # Each call runs on an isolated copy of the contract storage and is
        # committed only if it completes successfully (atomic call).
        contract_storage = Storage.from_dict(contract["storage"])
        vm = SANVirtualMachine(storage=contract_storage, gas_limit=gas_limit)

        call_bytecode = contract_bytecode + [
            *[item for arg in args for item in (OpCode.PUSH.value, arg)],
            OpCode.PUSH.value,
            function_name,
            OpCode.PUSH.value,
            len(args),
            OpCode.CALL_FUNC.value,
        ]

        # Start after the contract's top-level code: functions are already
        # registered in the storage snapshot, top-level init must not re-run.
        vm.run(call_bytecode, start_pc=len(contract_bytecode))
        self.last_gas_used = vm.gas_used
        self.last_logs = list(vm.logs)

        return_value = vm.stack[-1] if vm.stack else None

        contract["storage"] = contract_storage.to_dict()
        return return_value
