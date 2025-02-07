import time, json
import pqcrypto.sign.dilithium2 as dilithium2

class Transaction:
    def __init__(self, transaction_pool, data):
        self.timestamp = time.time()
        self.data = data
        self.fee = self.calculate_fee(transaction_pool)

    def calculate_fee(self, transaction_pool):
        """
        Dynamic transaction fee calculation:
        - As the transaction intensity increases, the fee increases.
        - Minimum fee: 0.01 per byte
        - Maximum fee: 0.1 per byte (If the transaction pool is very full)
        """

        base_fee_per_byte = 0.01  # Normal
        max_fee_per_byte = 0.1  # Busy

        pool_size = len(transaction_pool)

        congestion_factor = min(pool_size / 100, 10)  # 100 tx = * 2, max *10

        # Calculate fee
        dynamic_fee = base_fee_per_byte * (1 + congestion_factor)

        # Check max fee
        dynamic_fee = min(dynamic_fee, max_fee_per_byte)

        # Return fee
        return len(self.data.encode('utf-8')) * dynamic_fee

    @staticmethod
    def verify_transaction(tx_bytes) -> bool:
        """
        "signature" and "public_key" in the transaction data coming as tx_bytes
        It verifies signatures with the Dilithium2 algorithm using fields.

        tx_bytes: Transaction data serialized in JSON format and converted to bytes.
                  In this data, "public_key" and "signature" fields must be in hex string format.

        Return:
          True: If the signature is verified.
          False: If the signature cannot be verified.
        """

        # tx_bytes to JSON.
        try:
            tx = json.loads(tx_bytes.decode('utf-8'))
        except Exception as e:
            raise Exception("Transaction decoding error:", e)

        # Public key control end convert:
        if "sender" not in tx:
            raise Exception("Transaction does not have sender")

        public_key_hex = tx["sender"]
        try:
            public_key = bytes.fromhex(public_key_hex)
        except Exception as e:
            raise Exception("Public key convert error:", e)

        # Signature control and convert
        if "signature" not in tx:
            raise Exception("Transaction does not have signature")

        signature_hex = tx["signature"]
        try:
            signature = bytes.fromhex(signature_hex)
        except Exception as e:
            raise Exception("Signature dönüşüm hatası:", e)

        # remove signature
        tx_copy = tx.copy()
        del tx_copy["signature"]

        message_bytes = Transaction.serialize_message(tx_copy, exclude_signature=False)

        try:
            # Verify
            dilithium2.verify(message_bytes, signature, public_key)
            return True
        except Exception as e:
            raise Exception("Signature verify error:", e)

    @staticmethod
    def serialize_message(message: dict, exclude_signature: bool = True) -> bytes:
        """
        Serializes the message in JSON format.

        If exclude_signature is True, the 'signature' field is excluded from the message.
        This allows us to obtain consistent data during signing and verification processes.
        """
        message_to_sign = message.copy()
        if exclude_signature and "signature" in message_to_sign:
            del message_to_sign["signature"]

        # With sort_keys=True, the fields are placed in the same order every time.
        return json.dumps(message_to_sign, sort_keys=True).encode('utf-8')