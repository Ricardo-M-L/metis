export namespace main {

	export class ChatMessage {
	    Role: string;
	    Content: string;

	    static createFrom(source: any = {}) {
	        return new ChatMessage(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.Role = source["Role"];
	        this.Content = source["Content"];
	    }
	}
	export class MessageResponse {
	    text: string;
	    threadId: string;
	    warning?: string;
	    error?: string;

	    static createFrom(source: any = {}) {
	        return new MessageResponse(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.text = source["text"];
	        this.threadId = source["threadId"];
	        this.warning = source["warning"];
	        this.error = source["error"];
	    }
	}
	export class ScheduledTaskInfo {
	    ID: string;
	    Name: string;
	    Prompt: string;
	    Schedule: string;
	    Enabled: boolean;
	    Paused: boolean;
	    NextRun: string;
	    LastRun: string;
	    RunCount: number;
	    Repeat: number;
	    Silent: boolean;
	    SessionMode: string;

	    static createFrom(source: any = {}) {
	        return new ScheduledTaskInfo(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ID = source["ID"];
	        this.Name = source["Name"];
	        this.Prompt = source["Prompt"];
	        this.Schedule = source["Schedule"];
	        this.Enabled = source["Enabled"];
	        this.Paused = source["Paused"];
	        this.NextRun = source["NextRun"];
	        this.LastRun = source["LastRun"];
	        this.RunCount = source["RunCount"];
	        this.Repeat = source["Repeat"];
	        this.Silent = source["Silent"];
	        this.SessionMode = source["SessionMode"];
	    }
	}
	export class SessionInfo {
	    ID: string;
	    Title: string;
	    Model: string;
	    CreatedAt: string;

	    static createFrom(source: any = {}) {
	        return new SessionInfo(source);
	    }

	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ID = source["ID"];
	        this.Title = source["Title"];
	        this.Model = source["Model"];
	        this.CreatedAt = source["CreatedAt"];
	    }
	}

}
