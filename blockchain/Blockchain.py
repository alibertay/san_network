from blockchain.Block import Block

class Blockchain:
    def __init__(self):
        self.chain = []
        self.SAN = {}
        self._create_genesis_block()

    def _create_genesis_block(self):
        genesis_block = Block(
            index = 0,
            previous_block_hash = "0",
            validator = "GENESIS_VALIDATOR",
            validator_signature = "GENESIS_SIGNATURE",
            transactions = ["TEXT A MESSAGE TO THE HUMANITY"]
        )

        self.chain.append(genesis_block)

    def add_block(self, validator, validator_signature, transactions):
        last_block = self.chain[-1]

        new_block = Block(
            index = last_block.index + 1,
            previous_block_hash = last_block.current_block_hash,
            validator = validator,
            validator_signature = validator_signature,
            transactions = transactions
        )

        self.chain.append(new_block)