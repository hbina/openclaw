//! Rust implementation of OpenClaw's retained local-assistant runtime.
//!
//! Module boundaries mirror the production Go runtime so behavior can be
//! ported and verified package by package without changing the product contract.

pub mod channels;
pub mod cli;
pub mod config;
pub mod gateway;
pub mod maintenance;
pub mod memory;
pub mod providers;
pub mod state;
pub mod tools;
pub mod vector;
