# SAN Network Litepaper

## Introduction

SAN Network is a decentralized blockchain network designed for secure and scalable transaction validation. This network operates with a peer-to-peer (P2P) architecture, allowing nodes to synchronize, validate transactions, and maintain the integrity of the blockchain.

## Key Features

- **Fast and Secure Transactions**: Uses Dilithium2 cryptographic signatures to ensure transaction authenticity.
- **Dynamic Fee Model**: Transaction fees adjust dynamically based on network congestion.
- **Decentralized Peer Discovery**: Nodes automatically discover and synchronize with other peers in the network.
- **Gossip Protocol for Network Stability**: Efficient node communication ensures dead peers are identified and removed.
- **Block Validation Mechanism**: 66% of controller nodes must approve a block before it's added to the chain.

## Blockchain Architecture

SAN Network consists of the following core components:

### Blockchain

- Maintains an immutable chain of blocks.
- Includes a **Genesis Block** with an initial validator.
- Each block contains:
  - **Index**: Block number.
  - **Previous Block Hash**: Ensures chain integrity.
  - **Validator and Signature**: Signed with Dilithium2 algorithm.
  - **Transactions**: List of validated transactions.

### Transactions

- Transactions are signed using **Dilithium2** for post-quantum security.
- Dynamic fee calculation based on transaction pool size:
  - **Minimum Fee**: 0.01 per byte.
  - **Maximum Fee**: 0.1 per byte (when congestion is high).
- Signature verification ensures authenticity.

### Nodes

Each node in SAN Network performs the following functions:

- **Peer Discovery**: Connects to bootstrap nodes and expands its peer list.
- **Transaction Processing**: Accepts, verifies, and propagates transactions.
- **Block Validation**: Controls and approves new blocks before they are added to the chain.
- **Gossip Protocol**: Shares peer updates and block approvals across the network.

## Consensus and Block Creation

- A block is created when the total transaction fees in the pool exceed **500 SAN**.
- Validators sign the block using their private key.
- Controller nodes review and approve the block (66% consensus required).
- Approved blocks are broadcast to the network.

## Network Communication

SAN Network uses **FastAPI and WebSockets** for efficient communication:

- **API Endpoints**:
  - `/sync`: Synchronizes the blockchain state.
  - `/transaction`: Submits a new transaction.
  - `/bootstrap`: Retrieves peer list from bootstrap nodes.
  - `/join`: Allows a new node to join the network.

- **WebSocket Protocol**:
  - `PING/PONG`: Ensures peer activity.
  - `Gossip Messages`: Spreads updates about dead peers and new blocks.

- **Smart Contract Support**:
  - `SANVM`: SAN Network's virtual machine
  - `SANLANG`: Programming language for developing smart contracts / scripts for SANVM

## Running the SAN Network

### Requirements

Install dependencies with:

```sh
pip install -r requirements.txt
```
### Identify Private Key on Enviroment
```sh
PRIVATE_KEY=YOUR_PRIVATE_KEY
```
### Start Node
```sh
PRIVATE_KEY=YOUR_PRIVATE_KEY
```
This will start a FastAPI server for handling transactions and blockchain synchronization.

## Future Improvements
✅ Implementing staking mechanisms for validators.  
✅ Creating node groups


## Conclusion
SAN Network is a lightweight, scalable blockchain solution focused on decentralization, security, and performance. Its innovative approach to transaction validation and consensus makes it a powerful framework for secure digital transactions.
