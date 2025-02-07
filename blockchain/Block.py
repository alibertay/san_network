import time
import hashlib

class Block:
    def __init__(self, index, previous_block_hash, validator, validator_signature, transactions):
        self.index = index # Block index
        self.previous_block_hash = previous_block_hash # Previous block hash
        self.current_block_hash = self.calculate_hash() # Current block hash
        self.timestamp = time.time() # Now as epoch
        self.validator = validator # validator address
        self.validator_signature = validator_signature
        self.transactions = transactions # Transactions

    def calculate_hash(self):
        block_data = f"{self.index}{self.previous_block_hash}{self.timestamp}{self.transactions}"
        return hashlib.sha3_256(block_data.encode()).hexdigest()