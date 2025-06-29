import requests
import websockets
import asyncio
import pickle
import json
import random
import socket
import os
import pqcrypto.sign.dilithium2 as dilithium2

from blockchain.Blockchain import Blockchain
from blockchain.Transaction import Transaction
from blockchain.Block import Block

from SANVM.VM import SANVirtualMachine
from SANVM.Storage import Storage
from SANVM.pena_parser import PenaParser

from utils.parser import Parser

class Node:
    BLOCK_THRESHOLD_FEE = 500

    def __init__(self):
        self.PEERS = []

        self.incoming_node, self.outgoing_node = random.sample(self.PEERS, 2)
        self.controller_nodes = random.sample(self.PEERS, 10)

        self.check_dead_peers()

        self.blockchain = Blockchain()

        self.last_seen_block_timestamp = 0 # last seen block's timestamp

        self.storage = Storage()

        self.synchronize(self.last_seen_block_timestamp)

        self.transaction_pool = []

        self.vm = SANVirtualMachine(self.storage)

    async def check_dead_peers(self):
        """
        Controls Incoming and Outgoing Nodes. If someone died:
        1. Removes self.PEERS from the list.
        2. Determines the new Incoming and Outgoing Node.
        3. Notifies other nodes via Gossip.
        """
        dead_nodes = []
        for node in [self.incoming_node, self.outgoing_node]:
            is_alive = await self.ping_node(node)
            if not is_alive:
                dead_nodes.append(node)
                self.PEERS.remove(node)  # remove from list

        if dead_nodes:
            # choose new PEER
            self.incoming_node, self.outgoing_node = random.sample(self.PEERS, 2)

            # Tell dead peer with gossip
            for dead_peer in dead_nodes:
                await self.gossip_dead_peer(dead_peer)

    async def gossip_dead_peer(self, dead_peer):
        """
        It propagates dead peer information through the outgoing node.
        """
        message = json.dumps({"type": "DEAD_PEER", "peer": dead_peer})

        try:
            async with websockets.connect(f"ws://{self.outgoing_node}") as websocket:
                await websocket.send(message)
        except Exception as e:
            raise f"[ERROR] Could not gossip dead peer {dead_peer} to {self.outgoing_node}: {e}"

    async def listen_for_dead_peers(self, host="0.0.0.0", port=8771):
        """
        It listens to incoming "dead peer" messages and deletes them if the incoming peer is in the self.PEERS list.
        """

        async def handler(websocket, _):
            try:
                message = await websocket.recv()
                data = json.loads(message)

                if data.get("type") == "DEAD_PEER":
                    dead_peer = data.get("peer")
                    if dead_peer in self.PEERS:
                        self.PEERS.remove(dead_peer)

            except Exception as e:
                raise f"[ERROR] Failed to process dead peer update: {e}"

        async with websockets.serve(handler, host, port):
            await asyncio.Future()

    def start_dead_peer_listener(self):
        asyncio.run(self.listen_for_dead_peers())

    @staticmethod
    async def ping_node(node):
        """
        It pings a node, if the pong response comes it returns True, otherwise it returns False.
        """
        try:
            async with websockets.connect(f"ws://{node}") as websocket:
                await websocket.send(json.dumps({"type": "PING"}))
                response = await asyncio.wait_for(websocket.recv(), timeout=3)  # 3 sec timeout
                data = json.loads(response)
                return data.get("type") == "PONG"
        except Exception:
            return False

    @staticmethod
    def get_local_ip():
        """
        Get IP
        """
        try:
            s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
            s.connect(("8.8.8.8", 80))  # Google DNS
            local_ip = s.getsockname()[0]
            s.close()
            return local_ip
        except Exception as e:
            print(f"[ERROR] Could not get local IP: {e}")
            return "127.0.0.1"

    @staticmethod
    def discover_peers(bootstrap_node="127.0.0.1:6161"):
        """
        Yeni bir node başlatıldığında, Bootstrap Node'dan PEERS listesini alır.
        """
        try:
            response = requests.get(f"http://{bootstrap_node}/bootstrap")
            if response.status_code == 200:
                new_peers = response.json().get("peers", [])
                return new_peers
        except Exception as e:
            print(f"[ERROR] Could not fetch peers from {bootstrap_node}: {e}")

    async def register_to_network(self):
        """
        Report yourself to the Outgoing Node and have it added to the PEERS list.
        """
        message = json.dumps({"type": "PEER_UPDATE", "peer": self.get_local_ip})

        try:
            async with websockets.connect(f"ws://{self.outgoing_node}") as websocket:
                await websocket.send(message)
                print(f"[GOSSIP] Sent self to outgoing node {self.outgoing_node}.")
        except Exception as e:
            print(f"[ERROR] Could not register to network via {self.outgoing_node}: {e}")

    async def gossip_peers(self, new_peer):
        """
        When it learns a new PEER, it simply reports it to `outgoing_node`.
        The outgoing node will transmit this information to its outgoing (Gossip).
        """
        if not self.outgoing_node:
            print("[WARNING] No outgoing node set. Gossip cannot proceed.")
            return

        message = json.dumps({"type": "PEER_UPDATE", "peer": new_peer})

        try:
            async with websockets.connect(f"ws://{self.outgoing_node}") as websocket:
                await websocket.send(message)
                print(f"[GOSSIP] Sent new peer info to {self.outgoing_node}.")
        except Exception as e:
            raise f"[ERROR] Could not gossip new peer to {self.outgoing_node}: {e}"
# TODO: Global çağrı ile çözüm? 10 dk geride kalan node global call açar ve veri ister. Ama kimden/nasıl
    async def listen_for_peers(self, host="0.0.0.0", port=8770):
        """
        Start a WebSocket server that listens for PEERS updates.
        """

        async def handler(websocket, _):
            try:
                message = await websocket.recv()
                data = json.loads(message)

                if data.get("type") == "PEER_UPDATE":
                    new_peer = data.get("peer")
                    if new_peer not in self.PEERS:
                        self.PEERS.append(new_peer)
                        print(f"[PEER UPDATE] New peer added: {new_peer}")

                        # Gossip ile diğer node'lara yay
                        await self.gossip_peers(new_peer)

            except Exception as e:
                print(f"[ERROR] Peer update failed: {e}")

        async with websockets.serve(handler, host, port):
            print(f"[PEER SERVER] Listening on ws://{host}:{port}")
            await asyncio.Future()

    def start_peer_listener(self):
        asyncio.run(self.listen_for_peers())

    def synchronize(self, last_seen_block_timestamp):
        response = requests.get(f"https://{self.incoming_node}/sync?last_seen_block_timestamp={last_seen_block_timestamp}")
        nodes_block = response.json() if response.status_code == 200 else None

        if nodes_block and "block" in nodes_block and "storage" in nodes_block:
            new_blocks = [block for block in nodes_block["block"] if block.timestamp > last_seen_block_timestamp]
            if new_blocks:
                self.blockchain.chain.extend(new_blocks)

                self.storage = nodes_block["storage"]

        self.last_seen_block_timestamp = self.blockchain.chain[-1].timestamp

    def ask_synchronize(self, last_seen_block_timestamp):
        new_blocks = [block for block in self.blockchain.chain if block.timestamp > last_seen_block_timestamp]

        return {"block": new_blocks if new_blocks else None,
                "storage": self.storage if new_blocks else None}

    async def send_to_controllers(self, block):
        """
        Bloks send to controllers first
        If %66 of controllers approve it, gossip start
        """
        approvals = 0
        total_controllers = len(self.controller_nodes)

        if total_controllers == 0:
            raise "[WARNING] No controller nodes! Block cannot be verified."

        # Block pickle
        block_bytes = pickle.dumps(block)

        # Send to controller
        for controller in self.controller_nodes:
            try:
                async with websockets.connect(controller) as websocket:
                    await websocket.send(block_bytes)  # Pickle
                    response = await websocket.recv()  # Get answer

                    if response:
                        approvals += 1
            except Exception as e:
                raise f"[ERROR] Could not send block to {controller}: {e}"

        # %66
        approval_ratio = approvals / total_controllers
        if approval_ratio >= 0.66:
            await self.broadcast_block(block)
        else:
            raise f"[FAILED] Block rejected! Approval Ratio: {approval_ratio:.2f}"

    async def control_block(self, websocket, _):
        """
        Controller Nodes
        """
        try:
            # Get data
            block_bytes = await websocket.recv()
            block = pickle.loads(block_bytes)  # Pickle

            # Check block
            if self.verify_block(block):
                await websocket.send(True)
                return True
            else:
                await websocket.send(False)
                return False
        except Exception as e:
            print(f"[ERROR] Failed to control block: {e}")
            await websocket.send(False)
            return False

    def verify_block(self, block):
        """
        Validates transactions within the block.
        1. Are all transactions valid?
        2. Are the signatures of all transactions correct?
        3. Is the previous block hash correct?
        """
        for tx in block.transactions:
            if not Transaction.verify_transaction(tx):
                return False

        # Check hash
        last_block = self.blockchain.chain[-1]
        if block.previous_block_hash != last_block.current_block_hash:
            return False

        return True

    async def start_incoming_listener(self, host="0.0.0.0", port=8765):
        async def handler(websocket, _):
            async for message in websocket:
                received_block = pickle.loads(message)  # Byte to block
                self.blockchain.chain.append(received_block)

                self.update_SAN_balance_for_block(received_block)

        async with websockets.serve(handler, host, port):
            await asyncio.Future()

    def run_listener(self):
        asyncio.run(self.start_incoming_listener())

    async def run_controller_listener(self, host="0.0.0.0", port=8769):
        """
        Start controller websocket
        """
        async with websockets.serve(self.control_block, host, port):
            await asyncio.Future()

    def send_transaction(self, transaction: Transaction):
        self.transaction_pool.append(transaction)

        total_fee = sum(tx.fee for tx in self.transaction_pool)

        if total_fee >= self.BLOCK_THRESHOLD_FEE:
            last_block = self.blockchain.chain[-1]

            index = last_block.index + 1
            previous_block_hash = last_block.current_block_hash
            validator = Node.get_public_key()
            validator_signature = Node.sign_block(index, previous_block_hash, self.transaction_pool)

            new_block = Block(index, previous_block_hash, validator, validator_signature, self.transaction_pool)
            self.blockchain.chain.append(new_block)
            self.send_to_controllers(new_block)

            self.run_bytecodes_of_block(new_block)
            self.update_SAN_balance_for_block(new_block)
            self.transaction_pool = []

    async def broadcast_block(self, block):
        block_bytes = pickle.dumps(block)  # Block to the bytes

        try:
            async with websockets.connect(self.outgoing_node) as websocket:
                await websocket.send(block_bytes)
                print(f"[NODE] Sent block to {self.outgoing_node}")
        except Exception as e:
            print(f"[ERROR] Could not send block to {self.outgoing_node}: {e}")

    def run_contract_function_of_block(self, new_block):
        # contract_code = {"command": deploy, contract_id, bytecode}
        # contract_code = {"command": run, contract_id, function_name, params: []}

        for transaction in new_block.transactions:
            try:
                tx = json.loads(transaction.decode('utf-8'))
            except Exception as e:
                raise Exception("Transaction decoding error:", e)

            if "contract_code" in tx:
                try:
                    contract_code = tx["contract_code"]
                    command = contract_code["command"]

                    if command == "deploy":
                        if "pena_code" in contract_code:
                            parser = PenaParser()
                            bytecode = parser.parse(contract_code["pena_code"])
                            self.vm.deploy_contract(contract_code["contract_id"], bytecode)
                        else:
                            self.vm.deploy_contract(contract_code["contract_id"], contract_code["bytecode"])

                    elif command == "run":
                        self.vm.call_contract_function(
                            contract_code["contract_id"],
                            contract_code["function_name"],
                            contract_code.get("params", [])
                        )

                except Exception as e:
                    raise Exception(f"{e}")

    def run_bytecodes_of_block(self, new_block):
        """
        bytecode format:
        bytecode: [[PUSH, 10], [PUSH, 20], [ADD], [HALT]]
        :param new_block:

        """
        for transaction in new_block.transactions:
            try:
                tx = json.loads(transaction.decode('utf-8'))
            except Exception as e:
                raise Exception("Transaction decoding error:", e)

            if "bytecode" in tx:
                try:
                    # parse bytecode
                    bytecode = Parser.parse_instruction_list(tx["bytecode"])
                    SANVirtualMachine().run(bytecode)

                except Exception as e:
                    raise Exception(f"{e}")

    def update_SAN_balance_for_block(self, new_block):
        collected_fee = 0
        for transaction in new_block.transactions:
            collected_fee += transaction.fee

            try:
                tx = json.loads(transaction.decode('utf-8'))
            except Exception as e:
                raise Exception("Transaction decoding error:", e)

            if "value" in tx:
                sender = tx["sender"]
                receiver = tx["receiver"]
                value = tx["value"]

                if self.blockchain.SAN[sender] >= tx["value"] + transaction.fee:
                    self.blockchain.SAN[sender] -= value
                    self.blockchain.SAN[sender] -= transaction.fee
                    self.blockchain.SAN[receiver] += value
                else:
                    raise Exception("Not enough SAN")
            else:
                sender = tx["sender"]
                if self.blockchain.SAN[sender] >= transaction.fee:
                    self.blockchain.SAN[sender] -= transaction.fee
                else:
                    raise Exception("Not enough SAN")

        self.blockchain.SAN[new_block.validator] += collected_fee

    @staticmethod
    def sign_block(index, previous_block_hash, transactions):
        """
        Signs the given Block object with the Dilithium algorithm using private_key.

        For signing, the following fields of the block are used:
          -index
          - previous_block_hash
          -transactions

        The created signature is assigned to the block.validator_signature field in hex format.

        Args:
            block (Block): The block object to sign.
            private_key (bytes): The private key to be used for signing.
        """

        private_key = os.getenv("PRIVATE_KEY")

        # Create dict
        # Except validator signature
        data_to_sign = {
            "index": index,
            "previous_block_hash": previous_block_hash,
            "transactions": transactions
        }

        # Change to json
        message_bytes = json.dumps(data_to_sign, sort_keys=True).encode('utf-8')

        # Sign
        signature = dilithium2.sign(message_bytes, private_key)

        # Return sign
        return signature.hex()

    @staticmethod
    def get_public_key():
        """
        Extracts the public key from the given Dilithium2 private key.

        The secret key format of the Dilithium2 algorithm includes public key information at the end.
        Therefore, we can get the public key by taking the last dilithium2.PUBLICKEYBYTES byte of the private_key.

        Args:
            private_key (bytes): Dilithium2 private key.

        Returns:
            bytes: The obtained public key.

        Raises:
            ValueError: If private key size is different than expected.
        """

        private_key = os.getenv("PRIVATE_KEY")
        if len(private_key) != dilithium2.SECRET_KEY_SIZE:
            raise ValueError(
                f"Invalid private key size. Expecting: {dilithium2.SECRET_KEY_SIZE} bayt, "
                f"Get: {len(private_key)} bayt."
            )

        # Secret key's last section include public key
        public_key = private_key[-dilithium2.PUBLIC_KEY_SIZE:]
        return public_key