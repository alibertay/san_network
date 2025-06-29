# 🌐 SAN Network Litepaper

## 🧭 Introduction

SAN Network is a decentralized blockchain network designed for secure, scalable, and developer-friendly transaction validation. Operating with a peer-to-peer (P2P) architecture, SAN allows nodes to synchronize, validate transactions, and maintain blockchain integrity — all without centralized control.

> ✨ SAN now supports **smart contracts** written in the developer-friendly, high-level **PENA programming language**.

---

## 🔑 Key Features

- **Post-Quantum Security**: Uses **Dilithium2** cryptographic signatures to future-proof against quantum attacks.
- **Dynamic Fee Model**: Transaction fees scale with network load.
- **Decentralized Peer Discovery**: Nodes auto-discover peers and expand organically.
- **Gossip Protocol**: Maintains network health by removing unresponsive peers.
- **Smart Contract Support**: Build and deploy contracts in **PENA**, a lightweight high-level language compiled into SANVM bytecode.

---

## 🧱 Blockchain Architecture

### 🔗 Blockchain

- Immutable and verifiable chain of blocks.
- Starts with a **Genesis Block**.
- Each block includes:
  - **Index**
  - **Previous Block Hash**
  - **Validator & Signature** (Dilithium2)
  - **Transactions**
  - **Optional Smart Contract Execution Results**

### 💸 Transactions

- All transactions are signed using **Dilithium2**.
- **Dynamic Fees**:
  - Min: `0.01 SAN` per byte.
  - Max: `0.1 SAN` per byte (under high congestion).
- Signature verification is mandatory before mempool acceptance.

### 🖧 Nodes

Every SAN node performs:

- **Peer Discovery** via bootstrap servers.
- **Transaction Processing** & broadcasting.
- **Block Validation**: Each validator signs and verifies blocks.
- **Gossip Updates**: Share block/state info with peers.
- **Smart Contract Execution** via SANVM + PENA bytecode.

---

## 🔁 Consensus and Block Creation

- New block creation is triggered when mempool fees ≥ **500 SAN**.
- Validator signs the block.
- **66% consensus** required from controller nodes.
- Finalized blocks are broadcast to the network.

---

## 🔌 Network Communication

Uses **FastAPI** for REST + **WebSockets** for P2P events.

### 🔹 REST API Endpoints

- `POST /sync` → Blockchain sync  
- `POST /transaction` → Submit new TX  
- `GET /bootstrap` → Get known peers  
- `POST /join` → Join the network

### 🔸 WebSocket Protocol

- `PING/PONG` → Peer health check  
- `GOSSIP` → Propagate new blocks and peer status  
- `CONTRACT_EXEC` → Broadcast smart contract invocation results  

---

## 🔤 Smart Contracts with PENA

SAN supports high-level **PENA** language for smart contract development.

### ✍️ Example

```pena
function transferTokens(to, amount) {
  // token logic here
  print("Sending tokens to " + to)
}
