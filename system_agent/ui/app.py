"""
Streamlit Chat UI for the yuga.ai system agent.

This is the frontend layer - it only handles UI rendering.
All chat logic (memory retrieval, LLM calls) happens in the backend modules.

Run with: streamlit run system_agent/ui/app.py
"""

import json
import sys
import time
from pathlib import Path

import streamlit as st

# Add project root to path for imports
project_root = Path(__file__).parent.parent.parent
if str(project_root) not in sys.path:
    sys.path.insert(0, str(project_root))

from system_agent.brain.chatbot import ChatbotAgent
from system_agent.brain.memory import build_mem0_memory, clear_memories
from system_agent.brain.agent_service import create_agent, list_agents, ensure_agents_table
from system_agent.config.logging import setup_logging, get_logger

# Initialize logging
setup_logging()
logger = get_logger("ui")


def init_session_state():
    """Initialize Streamlit session state variables."""
    if "messages" not in st.session_state:
        st.session_state.messages = []
    
    if "agent" not in st.session_state:
        st.session_state.agent = None
    
    if "memory" not in st.session_state:
        st.session_state.memory = None
    
    if "user_id" not in st.session_state:
        st.session_state.user_id = None
    
    if "started" not in st.session_state:
        st.session_state.started = False
    
    # For agent creation workflow
    if "pending_agent_creation" not in st.session_state:
        st.session_state.pending_agent_creation = None
    
    if "show_dialog" not in st.session_state:
        st.session_state.show_dialog = False
    
    # For pending response generation
    if "pending_response" not in st.session_state:
        st.session_state.pending_response = None


def initialize_agent():
    """Initialize the agent and memory."""
    with st.spinner("Initializing yuga.ai system agent..."):
        logger.info("Initializing agent for user: %s", st.session_state.user_id)
        st.session_state.memory = build_mem0_memory()
        st.session_state.agent = ChatbotAgent(memory=st.session_state.memory)
        st.session_state.started = True
        
        # Ensure agents table exists
        try:
            ensure_agents_table()
        except Exception as e:
            logger.warning(f"Could not ensure agents table: {e}")


def render_landing_page():
    """Render the landing page with title, user input, and start button."""
    st.title(" yuga.ai ")
    st.caption("Agentic data platform with conversational memory, accelerated TTFT inference, and knowledge base management.")
    
    st.divider()
    
    # Center the form
    col1, col2, col3 = st.columns([1, 2, 1])
    
    with col2:
        # User name input
        user_name = st.text_input(
            "username",
            placeholder="Username...",
            key="user_name_input",
        )

        # Password input
        password = st.text_input(
            "password",
            placeholder="Password...",
            key="password_input",
            type="password",
        )
        
        # Start button
        if st.button("Login", use_container_width=True, type="primary"):
            if user_name.strip():
                st.session_state.user_id = user_name.strip()
                initialize_agent()
                st.rerun()
            else:
                st.error("Please enter your credentials to continue.")
    
    st.divider()
    st.caption("Powered by yuga.ai")


@st.dialog("Create a New Agent")
def show_create_agent_dialog():
    """Modal dialog for creating a new agent. Closes after submit."""
    st.write("Configure your new agent:")
    
    # Agent name input
    agent_name = st.text_input(
        "Agent Name",
        placeholder="Enter agent name...",
        key="dialog_agent_name",
    )
    
    # LLM provider selection
    model_provider = st.selectbox(
        "Select LLM Provider",
        options=["GPT-4o", "LLama-3.1-8B-Instruct", "DeepSeek-R1", "Qwen-2.5-32B", "Mistral-7B-Instruct", "Gemma-2-9b", "Any Hugging Face Model"],
        index=0,
        key="dialog_model_provider",
    )
    
    col1, col2 = st.columns(2)
    
    with col1:
        if st.button("Cancel", use_container_width=True):
            st.rerun()  # Close dialog
    
    with col2:
        if st.button("Create Agent", use_container_width=True, type="primary"):
            if not agent_name.strip():
                st.error("Please enter an agent name.")
            else:
                # Store data for later processing, then close dialog
                st.session_state.pending_agent_creation = {
                    "name": agent_name.strip(),
                    "provider": model_provider,
                }
                st.rerun()  # Close dialog - creation will happen in main page


def stream_text(text: str, delay: float = 0.03):
    """Generator that yields text character by character for st.write_stream."""
    for char in text:
        yield char
        time.sleep(delay)


def handle_pending_agent_creation():
    """
    Handle pending agent creation with progressive message display.
    
    Workflow:
    1. Show user message progressively
    2. After complete, trigger agent creation
    3. Show response
    """
    if not st.session_state.pending_agent_creation:
        return False
    
    agent_info = st.session_state.pending_agent_creation
    st.session_state.pending_agent_creation = None  # Clear immediately
    
    # Build the agent acknowledgment message
    agent_message = f"Creating {agent_info['name']} agent using {agent_info['provider']} model and embedder..."
    
    # Step 1: Display agent message progressively
    with st.chat_message("assistant"):
        st.write_stream(stream_text(agent_message, delay=0.02))
    
    # Add to message history
    st.session_state.messages.append({"role": "assistant", "content": agent_message})
    
    # Step 2: Create the agent
    with st.chat_message("assistant"):
        with st.spinner("Creating agent..."):
            result = create_agent(agent_info["name"], agent_info["provider"])
        
        # Step 3: Show response
        if result.get("success"):
            # Build the connection string
            connection_string = f"postgresql://127.0.0.1:5433/{result['db_name']}?user=yugabyte&password="
            
            # Create structured config
            agent_config = {
                "knowledge_base_connection_string": connection_string,
                "mem0_config": result['mem0_config']
            }
            
            response = f"""Agent **{result['name']}** created successfully!

**Connecting to the Agent:** 
- **URL to connect:** `https://localhost:8501/agent/{result['agent_id']}`

**Agent Details:**
- **ID:** `{result['agent_id']}`
- **Database:** `{result['db_name']}`
- **Graph:** `{result['graph_name']}`
- **Model Provider:** `{result['model_provider']}`

"""
            
            st.markdown(response)
            
            # Display configuration in collapsed expander
            with st.expander("📋 Agent Configuration Details", expanded=False):
                st.json(agent_config, expanded=1)
        else:
            response = f"""Failed to create agent.

**Error:** {result.get('error', 'Unknown error')}

Please check the logs for more details."""
            st.markdown(response)
    
    # Add to message history
    st.session_state.messages.append({"role": "assistant", "content": response})
    
    # Display follow-up actionable prompt
    follow_up_message = "Do you want to add a Knowledge Base to the agent? If yes, please provide the URL of the documents."
    with st.chat_message("assistant"):
        st.markdown(follow_up_message)
    st.session_state.messages.append({"role": "assistant", "content": follow_up_message})
    
    return True  # Indicate that we handled a creation


def handle_pending_response():
    """
    Handle pending response generation with thinking indicator.
    
    Workflow:
    1. User message is already in chat history
    2. Show thinking indicator
    3. Generate and display response
    """
    if not st.session_state.pending_response:
        return False
    
    response_info = st.session_state.pending_response
    st.session_state.pending_response = None  # Clear immediately
    
    # Generate response with spinner
    with st.chat_message("assistant"):
        with st.spinner("Thinking..."):
            response = st.session_state.agent.respond(
                user_input=response_info["prompt"],
                user_id=st.session_state.user_id
            )
        
        # Display response
        st.markdown(response)
        
        # Display image if present
        if "image_path" in response_info:
            st.image(response_info["image_path"], caption="Architecture Diagram", use_container_width=True)
    
    # Build assistant message
    agent_message = {
        "role": "assistant", 
        "content": response,
    }
    
    # Add image if present
    if "image_path" in response_info:
        agent_message["image_path"] = response_info["image_path"]
    
    st.session_state.messages.append(agent_message)
    
    return True  # Indicate that we handled a response


def render_sidebar():
    """Render the sidebar with action buttons."""
    with st.sidebar:
        st.header(" Agent Details ")
        
        st.write(f"**Logged in as user:** {st.session_state.user_id}")
        st.write(f"**Current Agent:** System Agent")
        
        st.divider()

        # Quick actions
        st.markdown("##### Quick Prompts")
        
        if st.button("💬 Create a new agent", use_container_width=True, type="secondary", key="create_agent_prompt_btn"):
            # Add the prompt to chat history as if user typed it
            prompt_text = "create a new agent"
            st.session_state.messages.append({"role": "user", "content": prompt_text})
            
            # Show the dialog
            st.session_state.show_dialog = True
            st.rerun()

        if st.button("💬 List all agents", use_container_width=True):
            prompt = "List all agents"
            st.session_state.messages.append({"role": "user", "content": prompt})
            
            # Fetch and display agents
            agents = list_agents()
            
            if agents:
                response = "**Available Agents:**\n\n"
                for agent in agents:
                    metadata = agent.get('metadata', {})
                    provider = metadata.get('model_provider', 'unknown') if metadata else 'unknown'
                    response += f"- **{agent['name']}** (ID: `{agent['id'][:8]}...`)\n"
                    response += f"  - Database: `{agent['db']}`\n"
                    response += f"  - Provider: `{provider}`\n\n"
            else:
                response = "No agents found. Create one using the 'Create a new Agent' button."
            
            st.session_state.messages.append({"role": "assistant", "content": response})
            st.rerun()

        if st.button("💬 Describe an Agent", use_container_width=True):    
            print("Describing an Agent...")

        if st.button("💬 What are the key architectural components of Yuga.ai?", use_container_width=True):
            prompt = "What are the key architectural components of Yuga.ai?"
            
            # Add user message
            user_message = {
                "role": "user", 
                "content": prompt,
            }
            st.session_state.messages.append(user_message)
            
            # Set pending response with image
            architecture_image_path = "system_agent/ui/assets/yuga_ai_architecture.png"
            st.session_state.pending_response = {
                "prompt": prompt,
                "image_path": architecture_image_path
            }
            st.rerun()
            

        # if st.button("💬 What are the key architectural components of Yuga.ai?", use_container_width=True):
        #     # Trigger architecture analysis in the chat
        #     architecture_image_path = "system_agent/ui/assets/yuga_ai_architecture.png"
        #     prompt = "Please analyze and describe the Agent memory architecture shown in the image. Explain the key components, data flow, and how they interact."
            
        #     # Add user message with image indicator
        #     user_message = {
        #         "role": "user", 
        #         "content": prompt,
        #         "image_path": architecture_image_path
        #     }
        #     st.session_state.messages.append(user_message)
            
        #     # Generate response with image
        #     with st.spinner("Retrieving architecture details..."):
        #         response = st.session_state.agent.respond_with_image(
        #             user_input=prompt,
        #             image_path=architecture_image_path,
        #             user_id=st.session_state.user_id
        #         )
            
        #     # Add assistant response
        #     st.session_state.messages.append({"role": "assistant", "content": response})
        #     st.rerun()
        

        st.divider()

        # Clear chat display
        if st.button("Clear Chat Display", use_container_width=True):
            st.session_state.messages = []
            st.rerun()
        
        # Clear memories (backend)
        if st.button("Clear All Memories", use_container_width=True, type="secondary"):
            if st.session_state.memory:
                if clear_memories(st.session_state.memory, st.session_state.user_id):
                    st.success("Memories cleared!")
                    logger.info("Memories cleared for user: %s", st.session_state.user_id)
                else:
                    st.error("Failed to clear memories")
        
        st.divider()
        st.caption("Powered by Yuga.AI")


def render_chat_messages():
    """Render all chat messages from session state."""
    for message in st.session_state.messages:
        with st.chat_message(message["role"]):
            # Check if message has an attached image
            if "image_path" in message:
                st.image(message["image_path"], caption="Architecture Diagram", use_container_width=True)
            st.markdown(message["content"])


def handle_user_input():
    """Handle new user input from chat."""
    if prompt := st.chat_input("Create agents, manage memory, or query the platform..."):
        # Add user message to display
        st.session_state.messages.append({"role": "user", "content": prompt})
        
        # Display user message immediately
        with st.chat_message("user"):
            st.markdown(prompt)
        
        # Generate response (all logic in backend)
        with st.chat_message("assistant"):
            with st.spinner("Thinking..."):
                # This calls the backend - memory retrieval, LLM call, etc.
                response = st.session_state.agent.respond(prompt, user_id=st.session_state.user_id)
            st.markdown(response)
        
        # Add assistant response to display
        st.session_state.messages.append({"role": "assistant", "content": response})


def render_chat_page():
    """Render the main chat interface."""
    # Render sidebar first
    render_sidebar()
    
    # Header with icon
    col1, col2 = st.columns([0.08, 0.92])
    with col1:
        st.image("system_agent/ui/assets/yugabyte_icon.png", width="content")
    with col2:
        st.title("yuga.ai")
    
    st.caption("Agentic data platform with conversational memory, accelerated TTFT inference, and knowledge base management.")
    
    # Show agent creation dialog if triggered by chat prompt
    if st.session_state.show_dialog:
        st.session_state.show_dialog = False  # Reset flag
        show_create_agent_dialog()

    # Render existing messages first
    render_chat_messages()
    
    # Handle pending agent creation (progressive message + creation)
    # This happens AFTER existing messages are rendered
    if handle_pending_agent_creation():
        pass  # Creation was handled, messages already displayed
    
    # Handle pending response (show thinking indicator and generate response)
    if handle_pending_response():
        pass  # Response was handled, messages already displayed
    
    # Handle new user input
    handle_user_input()


def main():
    """Main Streamlit app entry point."""

    # Page config
    st.set_page_config(
        page_title="yuga.ai | Agentic Memory Platform",
        page_icon="system_agent/ui/assets/yugabyte_icon.png",
        layout="wide",
        initial_sidebar_state="expanded",
    )
    
    # Initialize state
    init_session_state()
    
    # Show landing page or chat based on state
    if not st.session_state.started:
        render_landing_page()
    else:
        render_chat_page()


if __name__ == "__main__":
    main()
