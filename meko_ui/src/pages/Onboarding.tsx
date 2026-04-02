import { useState } from "react";
import { useLocation } from "wouter";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Tabs, TabsContent, TabsList, TabsTrigger } from "@/components/ui/tabs";
import { Check, Copy, ArrowRight, Terminal, MessageSquare, Sparkles, Map } from "lucide-react";

export default function Onboarding() {
  const [, setLocation] = useLocation();
  const [copiedStep, setCopiedField] = useState<string | null>(null);

  const handleCopy = (text: string, id: string) => {
    navigator.clipboard.writeText(text);
    setCopiedField(id);
    setTimeout(() => setCopiedField(null), 2000);
  };

  const handleComplete = () => {
    setLocation("/agent");
  };

  return (
    <div className="min-h-screen bg-background text-foreground flex flex-col items-center justify-center p-6 md:p-12">
      <div className="w-full max-w-4xl space-y-8 animate-in fade-in slide-in-from-bottom-8 duration-700">
        
        <div className="text-center space-y-3">
          <div className="inline-flex items-center gap-2 px-3 py-1 rounded-full bg-blue-500/10 text-blue-400 text-sm font-medium mb-2 border border-blue-500/20">
            <Sparkles className="w-4 h-4" />
            Getting Started
          </div>
          <h1 className="text-4xl md:text-5xl font-bold tracking-tight text-white">Explore your default datapack</h1>
          <p className="text-lg text-muted-foreground max-w-2xl mx-auto">
            Follow these quick steps to connect your preferred AI assistant to Meko and start storing long-term memories.
          </p>
        </div>

        <div className="grid gap-6">
          {/* Step 1 */}
          <Card className="border-border bg-[#1C2333] overflow-hidden">
            <div className="flex border-b border-border bg-[#0E1525] p-4 items-center gap-4">
              <div className="w-8 h-8 rounded-full bg-blue-600 flex items-center justify-center text-white font-bold shrink-0">1</div>
              <h2 className="text-xl font-semibold text-white">Connect to Meko Using MCP Server</h2>
            </div>
            <CardContent className="p-6">
              <Tabs defaultValue="claude" className="w-full">
                <TabsList className="grid h-auto w-full grid-cols-3 rounded-xl border border-blue-500/25 bg-[#0A1120] p-1.5 shadow-[inset_0_1px_0_rgba(255,255,255,0.05)]">
                  <TabsTrigger
                    value="claude"
                    className="h-9 w-full rounded-lg px-1.5 text-center text-xs font-semibold tracking-tight transition-all duration-200 sm:h-10 sm:px-2 sm:text-sm data-[state=active]:bg-blue-600 data-[state=active]:shadow-[0_6px_16px_rgba(37,99,235,0.35)]"
                  >
                    <span className="sm:hidden">Claude</span>
                    <span className="hidden sm:inline">Claude Desktop</span>
                  </TabsTrigger>
                  <TabsTrigger
                    value="openai"
                    className="h-9 w-full rounded-lg px-1.5 text-center text-xs font-semibold tracking-tight transition-all duration-200 sm:h-10 sm:px-2 sm:text-sm data-[state=active]:bg-blue-600 data-[state=active]:shadow-[0_6px_16px_rgba(37,99,235,0.35)]"
                  >
                    <span className="sm:hidden">OpenAI</span>
                    <span className="hidden sm:inline">OpenAI Codex</span>
                  </TabsTrigger>
                  <TabsTrigger
                    value="cursor"
                    className="h-9 w-full rounded-lg px-1.5 text-center text-xs font-semibold tracking-tight transition-all duration-200 sm:h-10 sm:px-2 sm:text-sm data-[state=active]:bg-blue-600 data-[state=active]:shadow-[0_6px_16px_rgba(37,99,235,0.35)]"
                  >
                    Cursor
                  </TabsTrigger>
                </TabsList>
                
                <TabsContent value="claude" className="mt-4 space-y-4">
                  <p className="text-sm text-muted-foreground">Add the following to your Claude Desktop configuration file (<code className="text-blue-300">claude_desktop_config.json</code>):</p>
                  <div className="relative">
                    <pre className="bg-[#0E1525] border border-border p-4 rounded-lg text-sm text-gray-300 overflow-x-auto font-mono">
{`{
  "mcpServers": {
    "meko": {
      "command": "npx",
      "args": ["-y", "@meko/mcp-server"],
      "env": {
        "MEKO_API_KEY": "meko_dp_default_secret_key"
      }
    }
  }
}`}
                    </pre>
                    <Button 
                      variant="ghost" 
                      size="icon" 
                      className="absolute top-3 right-3 text-muted-foreground hover:text-white bg-[#1C2333] border border-border"
                      onClick={() => handleCopy(`{\n  "mcpServers": {\n    "meko": {\n      "command": "npx",\n      "args": ["-y", "@meko/mcp-server"],\n      "env": {\n        "MEKO_API_KEY": "meko_dp_default_secret_key"\n      }\n    }\n  }\n}`, 'claude')}
                    >
                      {copiedStep === 'claude' ? <Check className="w-4 h-4 text-green-500" /> : <Copy className="w-4 h-4" />}
                    </Button>
                  </div>
                </TabsContent>
                
                <TabsContent value="openai" className="mt-4 space-y-4">
                  <p className="text-sm text-muted-foreground">Initialize the Meko MCP server in your OpenAI environment:</p>
                  <div className="relative">
                    <pre className="bg-[#0E1525] border border-border p-4 rounded-lg text-sm text-gray-300 overflow-x-auto font-mono">
{`export MEKO_API_KEY="meko_dp_default_secret_key"
npx @meko/mcp-server start`}
                    </pre>
                    <Button 
                      variant="ghost" 
                      size="icon" 
                      className="absolute top-3 right-3 text-muted-foreground hover:text-white bg-[#1C2333] border border-border"
                      onClick={() => handleCopy(`export MEKO_API_KEY="meko_dp_default_secret_key"\nnpx @meko/mcp-server start`, 'openai')}
                    >
                      {copiedStep === 'openai' ? <Check className="w-4 h-4 text-green-500" /> : <Copy className="w-4 h-4" />}
                    </Button>
                  </div>
                </TabsContent>

                <TabsContent value="cursor" className="mt-4 space-y-4">
                  <p className="text-sm text-muted-foreground">Add Meko to Cursor's settings under Features &gt; MCP:</p>
                  <div className="relative">
                    <pre className="bg-[#0E1525] border border-border p-4 rounded-lg text-sm text-gray-300 overflow-x-auto font-mono">
{`Name: Meko Datapack
Type: command
Command: npx -y @meko/mcp-server
Env: MEKO_API_KEY=meko_dp_default_secret_key`}
                    </pre>
                    <Button 
                      variant="ghost" 
                      size="icon" 
                      className="absolute top-3 right-3 text-muted-foreground hover:text-white bg-[#1C2333] border border-border"
                      onClick={() => handleCopy(`Name: Meko Datapack\nType: command\nCommand: npx -y @meko/mcp-server\nEnv: MEKO_API_KEY=meko_dp_default_secret_key`, 'cursor')}
                    >
                      {copiedStep === 'cursor' ? <Check className="w-4 h-4 text-green-500" /> : <Copy className="w-4 h-4" />}
                    </Button>
                  </div>
                </TabsContent>
              </Tabs>
            </CardContent>
          </Card>

          {/* Step 2 & 3 */}
          <Card className="border-border bg-[#1C2333] overflow-hidden">
            <div className="flex border-b border-border bg-[#0E1525] p-4 items-center gap-4">
              <div className="w-8 h-8 rounded-full bg-blue-600 flex items-center justify-center text-white font-bold shrink-0">2</div>
              <h2 className="text-xl font-semibold text-white">Store your first memory</h2>
            </div>
            <CardContent className="p-6 space-y-4">
              <p className="text-sm text-muted-foreground mb-2">Provide a user prompt to input in the chat for storing memories in Meko. Try pasting this specific preference:</p>
              
              <div className="flex items-center justify-between p-4 rounded-lg bg-[#0E1525] border border-border">
                <div className="flex items-center gap-3">
                  <MessageSquare className="w-5 h-5 text-purple-400" />
                  <span className="text-sm font-medium text-gray-200">"Please remember that I like to take vacations at tropical beach locations, on a budget."</span>
                </div>
                <Button 
                  variant="outline" 
                  size="sm" 
                  className="gap-2 border-border bg-[#1C2333] hover:bg-muted text-muted-foreground"
                  onClick={() => handleCopy("Please remember that I like to take vacations at tropical beach locations, on a budget.", 'memory')}
                >
                  {copiedStep === 'memory' ? <Check className="w-4 h-4 text-green-500" /> : <Copy className="w-4 h-4" />}
                  Copy
                </Button>
              </div>
            </CardContent>
          </Card>

          {/* Step 4 & 5 */}
          <Card className="border-border bg-[#1C2333] overflow-hidden">
            <div className="flex border-b border-border bg-[#0E1525] p-4 items-center gap-4">
              <div className="w-8 h-8 rounded-full bg-blue-600 flex items-center justify-center text-white font-bold shrink-0">3</div>
              <h2 className="text-xl font-semibold text-white">Test the integration</h2>
            </div>
            <CardContent className="p-6 space-y-6">
              <p className="text-sm text-muted-foreground">Start a new chat session and ask your AI to propose a vacation. Notice how the response improves when memory is used through Meko.</p>
              
              <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
                {/* Without Meko */}
                <div className="rounded-xl border border-border bg-[#0E1525] p-4 flex flex-col gap-4 relative overflow-hidden">
                  <div className="absolute top-0 right-0 px-3 py-1 bg-muted/50 rounded-bl-lg text-[10px] font-semibold text-muted-foreground uppercase tracking-wider border-b border-l border-border">
                    Without Meko
                  </div>
                  
                  <div className="flex gap-3 mt-4">
                    <div className="w-6 h-6 rounded-full bg-blue-600 flex items-center justify-center shrink-0 mt-1">
                      <span className="text-[10px] text-white font-medium">You</span>
                    </div>
                    <div className="bg-[#1C2333] border border-border px-3 py-2 rounded-2xl rounded-tl-sm text-sm text-gray-200">
                      Propose a 5 day vacation for me.
                    </div>
                  </div>

                  <div className="flex gap-3">
                    <div className="w-6 h-6 rounded-md bg-[#1C2333] border border-border flex items-center justify-center shrink-0 mt-1">
                      <Terminal className="w-3.5 h-3.5 text-muted-foreground" />
                    </div>
                    <div className="px-3 py-2 text-sm text-muted-foreground leading-relaxed">
                      I'd be happy to help you plan a vacation! To give you the best recommendations, could you tell me:
                      <ul className="list-disc pl-4 mt-2 space-y-1">
                        <li>What type of destination do you prefer?</li>
                        <li>What is your budget?</li>
                      </ul>
                    </div>
                  </div>
                </div>

                {/* With Meko */}
                <div className="rounded-xl border border-blue-500/30 bg-blue-500/5 p-4 flex flex-col gap-4 relative overflow-hidden">
                  <div className="absolute top-0 right-0 px-3 py-1 bg-blue-500 text-[10px] font-semibold text-white uppercase tracking-wider rounded-bl-lg shadow-lg">
                    With Meko
                  </div>
                  
                  <div className="flex gap-3 mt-4">
                    <div className="w-6 h-6 rounded-full bg-blue-600 flex items-center justify-center shrink-0 mt-1">
                      <span className="text-[10px] text-white font-medium">You</span>
                    </div>
                    <div className="bg-[#1C2333] border border-border px-3 py-2 rounded-2xl rounded-tl-sm text-sm text-gray-200">
                      Propose a 5 day vacation for me.
                    </div>
                  </div>

                  <div className="flex gap-3">
                    <div className="w-6 h-6 rounded-md bg-blue-600 border border-blue-500 flex items-center justify-center shrink-0 mt-1 shadow-[0_0_10px_rgba(37,99,235,0.5)]">
                      <Sparkles className="w-3.5 h-3.5 text-white" />
                    </div>
                    <div className="group px-3 py-2 text-sm text-gray-200 leading-relaxed">
                      <span className="mb-2 inline-flex items-center rounded-full border border-blue-400/40 bg-blue-500/15 px-2.5 py-1 text-[11px] font-semibold tracking-wide text-blue-200 md:hidden">
                        Memory: budget-friendly tropical beaches
                      </span>
                      <span className="mb-2 hidden items-center rounded-full border border-blue-400/40 bg-blue-500/15 px-2.5 py-1 text-[11px] font-semibold tracking-wide text-blue-200 opacity-0 translate-y-1 transition-all duration-300 group-hover:translate-y-0 group-hover:opacity-100 md:inline-flex">
                        Memory used: budget-friendly tropical beaches
                      </span>
                      <br className="hidden md:block" />
                      Using your Meko memory, I recommend a 5-day trip to Puerto Escondido, Mexico! 🏖️
                      <br/><br/>
                      Here is a cost-effective itinerary that maximizes your beach time...
                    </div>
                  </div>
                </div>
              </div>

              <div className="flex items-center justify-between p-4 rounded-lg bg-[#0E1525] border border-border mt-4">
                <div className="flex items-center gap-3">
                  <Map className="w-5 h-5 text-amber-400" />
                  <span className="text-sm font-medium text-gray-200">"Propose a 5 day vacation for me."</span>
                </div>
                <Button 
                  variant="outline" 
                  size="sm" 
                  className="gap-2 border-border bg-[#1C2333] hover:bg-muted text-muted-foreground"
                  onClick={() => handleCopy("Propose a 5 day vacation for me.", 'vacation')}
                >
                  {copiedStep === 'vacation' ? <Check className="w-4 h-4 text-green-500" /> : <Copy className="w-4 h-4" />}
                  Copy Prompt
                </Button>
              </div>
            </CardContent>
          </Card>
        </div>

        <div className="flex justify-center pt-4 pb-12">
          <Button size="lg" className="gap-2 bg-white text-black hover:bg-gray-200 px-8 py-6 text-lg font-semibold rounded-full" onClick={handleComplete}>
            Go to Meko Agent Dashboard
            <ArrowRight className="w-5 h-5" />
          </Button>
        </div>

      </div>
    </div>
  );
}
