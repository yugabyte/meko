"""LangChain-based conversational agent with mem0 memory."""

from __future__ import annotations

import base64
import threading
from dataclasses import dataclass, field
from pathlib import Path
from typing import Optional

from langchain_openai import ChatOpenAI
from langchain_core.messages import HumanMessage, SystemMessage

try:
    from system_agent.brain.memory import (
        add_memory,
        format_memory_items,
        search_memories,
    )
    from system_agent.config.settings import get_settings
    from system_agent.config.logging import get_logger
except ModuleNotFoundError:  # Allow running from within the cli/ directory.
    from system_agent.brain.memory import (
        add_memory,
        format_memory_items,
        search_memories,
    )
    from config.settings import get_settings
    from config.logging import get_logger


logger = get_logger("chatbot")


def load_platform_info() -> str:
    """Load yuga.ai platform details from knowledge base."""
    knowledge_base_path = Path(__file__).parent / "knowledge_base" / "yuga_ai_details.txt"
    try:
        return knowledge_base_path.read_text(encoding="utf-8")
    except FileNotFoundError:
        logger.warning(f"Knowledge base file not found: {knowledge_base_path}")
        return "yuga.ai is an AI data platform for building agentic applications."


# Load platform info once at module level
PLATFORM_INFO = load_platform_info()


@dataclass
class ChatbotAgent:
    memory: object
    _pending_adds: int = field(default=0, init=False)
    _add_lock: threading.Lock = field(default_factory=threading.Lock, init=False)
    _add_complete: threading.Event = field(default_factory=threading.Event, init=False)

    def __post_init__(self) -> None:
        settings = get_settings()
        self.system_prompt = settings.system_prompt
        self.llm = ChatOpenAI(
            model=settings.openai_model,
            api_key=settings.openai_api_key,
        )
        # Initialize vision-capable LLM for image analysis
        self.vision_llm = ChatOpenAI(
            model="gpt-4o",  # GPT-4 with vision capabilities
            api_key=settings.openai_api_key,
        )
        # Initially, no pending adds
        self._add_complete.set()

    def load_platform_info() -> str:
        """Load yuga.ai platform details from knowledge base."""
        knowledge_base_path = Path(__file__).parent / "knowledge_base" / "yuga_ai_details.txt"
        try:
            return knowledge_base_path.read_text(encoding="utf-8")
        except FileNotFoundError:
            logger.warning(f"Knowledge base file not found: {knowledge_base_path}")
            return "yuga.ai is an AI data platform for building agentic applications."

    # Load platform info once at module level
    PLATFORM_INFO = load_platform_info()

    def _add_memory_background(
        self,
        text: str,
        user_id: str,
        metadata: Optional[dict] = None,
    ) -> None:
        """Add memory in background thread."""
        try:
            add_memory(self.memory, text, user_id=user_id, metadata=metadata)
        finally:
            with self._add_lock:
                self._pending_adds -= 1
                if self._pending_adds == 0:
                    self._add_complete.set()
                    logger.debug("All pending memory adds complete")

    def _schedule_add_memory(
        self,
        text: str,
        user_id: str,
        metadata: Optional[dict] = None,
    ) -> None:
        """Schedule memory add to run in background thread."""
        with self._add_lock:
            self._pending_adds += 1
            self._add_complete.clear()
        
        thread = threading.Thread(
            target=self._add_memory_background,
            args=(text, user_id, metadata),
            daemon=True,
        )
        thread.start()
        logger.debug("Scheduled background memory add | pending=%d", self._pending_adds)

    def _wait_for_pending_adds(self, timeout: float = 30.0) -> bool:
        """Wait for any pending memory adds to complete."""
        if self._add_complete.is_set():
            return True
        
        logger.debug("Waiting for pending memory adds to complete...")
        result = self._add_complete.wait(timeout=timeout)
        if result:
            logger.debug("Pending memory adds completed")
        else:
            logger.warning("Timeout waiting for pending memory adds")
        return result

    def respond(self, user_input: str, user_id: str) -> str:
        """
        Generate a response to user input.
        
        Flow:
        1. Wait for any pending memory adds from previous turn
        2. Search for relevant memories
        3. Call LLM with context
        4. Schedule memory adds for this turn (background)
        5. Return response immediately
        """
        # Wait for previous adds to complete before searching
        self._wait_for_pending_adds()
        
        # Search memories (now safe - previous adds are complete)
        memories = search_memories(self.memory, user_input, user_id=user_id, limit=5)
        memory_text = format_memory_items(memories)

        # Build messages
        messages = [SystemMessage(content=self.system_prompt)]
        if memory_text:
            messages.append(
                SystemMessage(content=f"Relevant past information:\n{memory_text}")
            )
        messages.append(SystemMessage(content=f"if any questions are asked about the yuga.ai platform, use the following information to answer:"))
        messages.append(SystemMessage(content=f"yuga.ai platform details:\n\n{PLATFORM_INFO}"))
        messages.append(SystemMessage(content=f"if any of the following questions are asked, who are you?, What are the key architectural components of Yuga.ai?, What are Yuga.ai key capabilities? respond exactly as provided in the context."))
        messages.append(SystemMessage(content=f"if any of the following questions are asked, Can you add my documents present at s3://proto-automated-embedding/playgentic_documents/sample/ to the knowledge base. Respond saying, Vector index creation kicked off for pep_ai_index_ba480fe7"))
        messages.append(HumanMessage(content=user_input))

        # Log the full prompt being sent to LLM
        logger.info("=" * 80)
        logger.info("PROMPT BEING SENT TO LLM:")
        logger.info("=" * 80)
        for i, msg in enumerate(messages, 1):
            role = msg.__class__.__name__.replace("Message", "")
            logger.info(f"\n[Message {i} - {role}]")
            logger.info(f"{msg.content}")
            logger.info("-" * 80)
        logger.info("=" * 80)

        # Call LLM
        response = self.llm.invoke(messages)
        answer = response.content if hasattr(response, "content") else str(response)

        # Schedule memory adds in background (non-blocking)
        self._schedule_add_memory(user_input, user_id=user_id, metadata={"role": "user"})
        self._schedule_add_memory(answer, user_id=user_id, metadata={"role": "assistant"})

        return answer
    
    def respond_with_image(
        self, 
        user_input: str, 
        image_path: str | Path, 
        user_id: str
    ) -> str:
        """
        Generate a response with image context.
        
        Args:
            user_input: The user's text input/question
            image_path: Path to the image file
            user_id: User identifier
            
        Returns:
            LLM response as string
        """
        # Wait for previous adds to complete
        self._wait_for_pending_adds()
        
        # Read and encode the image
        image_path = Path(image_path)
        if not image_path.exists():
            logger.error(f"Image not found: {image_path}")
            return f"Error: Image file not found at {image_path}"
        
        with open(image_path, "rb") as image_file:
            image_data = base64.b64encode(image_file.read()).decode("utf-8")
        
        # Determine image format
        image_format = image_path.suffix.lstrip(".").lower()
        if image_format == "jpg":
            image_format = "jpeg"
        
        # Search for relevant memories
        memories = search_memories(self.memory, user_input, user_id=user_id, limit=5)
        memory_text = format_memory_items(memories)
        
        # Build messages with image
        messages = [SystemMessage(content=self.system_prompt)]
        if memory_text:
            messages.append(
                SystemMessage(content=f"Relevant past information:\n{memory_text}")
            )
        
        # Create message with both image and text
        message_content = [
            {
                "type": "image_url",
                "image_url": {
                    "url": f"data:image/{image_format};base64,{image_data}"
                }
            },
            {
                "type": "text",
                "text": user_input
            }
        ]
        messages.append(HumanMessage(content=message_content))
        
        # Call vision LLM
        logger.info(f"Calling vision LLM with image: {image_path.name}")
        response = self.vision_llm.invoke(messages)
        answer = response.content if hasattr(response, "content") else str(response)
        
        # Schedule memory adds in background
        self._schedule_add_memory(
            f"{user_input} [Image: {image_path.name}]", 
            user_id=user_id, 
            metadata={"role": "user", "has_image": True}
        )
        self._schedule_add_memory(answer, user_id=user_id, metadata={"role": "assistant"})
        
        return answer
