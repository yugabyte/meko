import { useState, useRef, useEffect } from "react";
import { Send, Bot, Database, User, Settings2, ActivitySquare, BrainCircuit, Search, Bell } from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";
import { ScrollArea } from "@/components/ui/scroll-area";
import { Avatar, AvatarFallback } from "@/components/ui/avatar";

// Mock Data
const DATAPACKS = [
  { id: "dp_1", name: "meko_default_datapack" },
  { id: "dp_2", name: "customer_support_db" },
];

const SCOPES = [
  { id: "admin", name: "Admin" },
  { id: "read_write", name: "Read & Write" },
  { id: "read_only", name: "Read Only" },
  { id: "observe", name: "Observe" },
];

const RECENT_MEMORIES = [
  { id: "m1", agent: "meko agent", action: "Stored user preference", detail: "Dark mode enabled", time: "2 min ago" },
  { id: "m2", agent: "data_indexer", action: "Indexed new document", detail: "Stripe API v2 docs", time: "15 min ago" },
  { id: "m3", agent: "meko agent", action: "Created calendar event", detail: "Team sync at 3PM", time: "1 hour ago" },
  { id: "m4", agent: "query_assistant", action: "Executed analytical query", detail: "Monthly active users", time: "3 hours ago" },
  { id: "m5", agent: "data_indexer", action: "Updated vector store", detail: "150 new embeddings", time: "5 hours ago" },
];

interface Message {
  id: string;
  role: "user" | "assistant";
  content: string;
}

export default function MekoAgent() {
  const [activeDatapack, setActiveDatapack] = useState(DATAPACKS[0].id);
  const [activeScope, setActiveScope] = useState(SCOPES[0].id);
  const [input, setInput] = useState("");
  const [messages, setMessages] = useState<Message[]>([
    {
      id: "msg_1",
      role: "assistant",
      content: "Hello! I'm the `meko agent` connected to your datapack. How can I help you manage your agent data, memories, or knowledge bases today?"
    }
  ]);
  
  const scrollRef = useRef<HTMLDivElement>(null);

  useEffect(() => {
    if (scrollRef.current) {
      scrollRef.current.scrollTop = scrollRef.current.scrollHeight;
    }
  }, [messages]);

  const handleSend = () => {
    if (!input.trim()) return;

    const userMsg: Message = { id: Date.now().toString(), role: "user", content: input };
    setMessages(prev => [...prev, userMsg]);
    setInput("");

    // Mock bot response
    setTimeout(() => {
      const botMsg: Message = {
        id: (Date.now() + 1).toString(),
        role: "assistant",
        content: `I've received your query regarding the \`${DATAPACKS.find(d => d.id === activeDatapack)?.name}\` datapack operating under \`${activeScope}\` scope. As a mockup, I cannot process live data right now, but I would execute this operation mapping to your PostgreSQL logical DB.`
      };
      setMessages(prev => [...prev, botMsg]);
    }, 1000);
  };

  return (
    <div className="flex-1 h-screen flex bg-background text-foreground overflow-hidden">
      
      {/* Main Chat Area */}
      <div className="flex-1 flex flex-col min-w-0 border-r border-border">
        {/* Header */}
        <header className="flex items-center justify-between px-6 py-4 border-b border-border bg-background/95 backdrop-blur z-10 shrink-0">
          <div className="flex items-center gap-4">
            <div className="flex items-center gap-2">
              <Bot className="w-5 h-5 text-blue-400" />
              <h1 className="text-lg font-semibold tracking-tight text-white">Meko Agent</h1>
            </div>
            <div className="h-4 w-px bg-border mx-2"></div>
            <div className="flex items-center gap-3">
              <Select value={activeDatapack} onValueChange={setActiveDatapack}>
                <SelectTrigger className="h-8 w-[200px] bg-muted/50 border-border text-xs focus:ring-0 focus:ring-offset-0">
                  <div className="flex items-center gap-2">
                    <Database className="w-3.5 h-3.5 text-muted-foreground" />
                    <SelectValue placeholder="Select Datapack" />
                  </div>
                </SelectTrigger>
                <SelectContent>
                  {DATAPACKS.map(dp => (
                    <SelectItem key={dp.id} value={dp.id} className="text-xs">
                      {dp.name}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>

              <Select value={activeScope} onValueChange={setActiveScope}>
                <SelectTrigger className="h-8 w-[140px] bg-muted/50 border-border text-xs focus:ring-0 focus:ring-offset-0">
                  <div className="flex items-center gap-2">
                    <Settings2 className="w-3.5 h-3.5 text-muted-foreground" />
                    <SelectValue placeholder="Scope" />
                  </div>
                </SelectTrigger>
                <SelectContent>
                  {SCOPES.map(s => (
                    <SelectItem key={s.id} value={s.id} className="text-xs">
                      {s.name}
                    </SelectItem>
                  ))}
                </SelectContent>
              </Select>
            </div>
          </div>
          
          <div className="flex items-center gap-3">
            <div className="text-xs text-muted-foreground flex items-center gap-1.5 px-3 py-1.5 bg-green-500/10 rounded-full border border-green-500/20">
              <span className="w-1.5 h-1.5 rounded-full bg-green-500"></span>
              Agent Active
            </div>
          </div>
        </header>

        {/* Chat Messages */}
        <div 
          className="flex-1 overflow-y-auto p-6 space-y-6 bg-[#0E1525]"
          ref={scrollRef}
        >
          {messages.map((msg) => (
            <div key={msg.id} className={`flex gap-4 max-w-3xl ${msg.role === "user" ? "ml-auto flex-row-reverse" : ""}`}>
              <Avatar className={`w-8 h-8 rounded-md shrink-0 border ${msg.role === 'assistant' ? 'bg-[#1C2333] border-border' : 'bg-blue-600 border-blue-500'}`}>
                {msg.role === "assistant" ? (
                  <Bot className="w-4 h-4 text-blue-400" />
                ) : (
                  <User className="w-4 h-4 text-white" />
                )}
              </Avatar>
              
              <div className={`flex flex-col gap-1 ${msg.role === "user" ? "items-end" : "items-start"}`}>
                <div className="flex items-center gap-2">
                  <span className="text-xs font-medium text-muted-foreground">
                    {msg.role === "assistant" ? "meko agent" : "You"}
                  </span>
                </div>
                <div className={`px-4 py-3 rounded-2xl text-sm ${
                  msg.role === "user" 
                    ? "bg-blue-600 text-white rounded-tr-sm" 
                    : "bg-[#1C2333] text-gray-200 border border-border rounded-tl-sm leading-relaxed"
                }`}>
                  {msg.content}
                </div>
              </div>
            </div>
          ))}
        </div>

        {/* Input Area */}
        <div className="p-4 bg-background border-t border-border shrink-0">
          <div className="max-w-4xl mx-auto relative flex items-center">
            <Input 
              value={input}
              onChange={(e) => setInput(e.target.value)}
              onKeyDown={(e) => e.key === 'Enter' && handleSend()}
              placeholder="Ask meko agent to query data, update memories, or manage vectors..."
              className="w-full bg-[#1C2333] border-border h-12 pl-4 pr-12 text-sm rounded-xl focus-visible:ring-1 focus-visible:ring-blue-500 focus-visible:ring-offset-0 text-white placeholder:text-muted-foreground"
            />
            <Button 
              size="icon" 
              className="absolute right-1.5 h-9 w-9 bg-blue-600 hover:bg-blue-700 text-white rounded-lg transition-colors"
              onClick={handleSend}
              disabled={!input.trim()}
            >
              <Send className="w-4 h-4 ml-0.5" />
            </Button>
          </div>
          <div className="max-w-4xl mx-auto mt-2 flex items-center justify-center gap-4 text-xs text-muted-foreground">
            <span>Press <kbd className="px-1.5 py-0.5 bg-muted rounded border border-border">Enter</kbd> to send</span>
          </div>
        </div>
      </div>

      {/* Right Sidebar - Notifications & Memories */}
      <div className="w-[320px] shrink-0 bg-background flex flex-col">
        <div className="px-5 py-4 border-b border-border bg-background/95 backdrop-blur z-10 shrink-0">
          <h3 className="font-medium text-white flex items-center gap-2">
            <ActivitySquare className="w-4 h-4 text-purple-400" />
            Live Activity
          </h3>
        </div>

        <ScrollArea className="flex-1">
          <div className="p-5 space-y-6">
            <div>
              <h4 className="text-xs font-semibold text-muted-foreground uppercase tracking-wider mb-4 flex items-center gap-2">
                <BrainCircuit className="w-3.5 h-3.5" />
                Recent Agent Memories
              </h4>
              <div className="space-y-4">
                {RECENT_MEMORIES.map((memory) => (
                  <div key={memory.id} className="relative pl-4 border-l border-border/50 hover:border-blue-500/50 transition-colors">
                    <div className="absolute w-2 h-2 rounded-full bg-blue-500/50 -left-[4.5px] top-1.5 border border-background"></div>
                    <div className="flex flex-col gap-1">
                      <div className="flex items-center justify-between">
                        <span className="text-xs font-medium text-blue-400">{memory.agent}</span>
                        <span className="text-[10px] text-muted-foreground">{memory.time}</span>
                      </div>
                      <p className="text-sm text-gray-300">{memory.action}</p>
                      <p className="text-xs text-muted-foreground bg-muted/30 px-2 py-1 rounded inline-block w-fit mt-0.5">
                        {memory.detail}
                      </p>
                    </div>
                  </div>
                ))}
              </div>
            </div>

            <div className="pt-4 border-t border-border">
              <Button variant="outline" className="w-full text-xs text-muted-foreground border-dashed bg-transparent hover:bg-muted hover:text-white">
                View All Activity
              </Button>
            </div>
          </div>
        </ScrollArea>
      </div>

    </div>
  );
}
