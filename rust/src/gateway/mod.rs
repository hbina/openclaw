mod agent;
mod conversation_lock;
mod history;
mod rag;
mod server;

pub use agent::{Agent, AgentError, ChatInput, PreparedResponse};
pub use conversation_lock::{ConversationGuard, ConversationLockManager};
pub use history::{
    PersistedInboundMessage, PersistedScheduledReminder, history_message, reconstruct_history,
    render_inbound_message, render_scheduled_reminder,
};
pub use rag::{
    ConversationExchange, RagError, RagRetrievalResult, RagService, recent_conversation,
};
pub(crate) use rag::{complete_exchanges, insert_archive_message};
pub use server::{Gateway, GatewayError};
