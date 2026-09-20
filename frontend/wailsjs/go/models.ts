export namespace ipc {
	
	export class DiffDTO {
	    missingInHosts: string[];
	    missingInConfig: string[];
	    consistent: boolean;
	
	    static createFrom(source: any = {}) {
	        return new DiffDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.missingInHosts = source["missingInHosts"];
	        this.missingInConfig = source["missingInConfig"];
	        this.consistent = source["consistent"];
	    }
	}
	export class DomainDTO {
	    id: string;
	    domain: string;
	
	    static createFrom(source: any = {}) {
	        return new DomainDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.domain = source["domain"];
	    }
	}
	export class ForwardDTO {
	    id: string;
	    listenPort: number;
	    targetHost: string;
	    targetPort: number;
	    enabled: boolean;
	
	    static createFrom(source: any = {}) {
	        return new ForwardDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.id = source["id"];
	        this.listenPort = source["listenPort"];
	        this.targetHost = source["targetHost"];
	        this.targetPort = source["targetPort"];
	        this.enabled = source["enabled"];
	    }
	}
	export class StatusInfoDTO {
	    state: string;
	    reason: string;
	    resolvedIP: string;
	
	    static createFrom(source: any = {}) {
	        return new StatusInfoDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.state = source["state"];
	        this.reason = source["reason"];
	        this.resolvedIP = source["resolvedIP"];
	    }
	}
	export class ForwardStatusDTO {
	    forward: ForwardDTO;
	    status: StatusInfoDTO;
	
	    static createFrom(source: any = {}) {
	        return new ForwardStatusDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.forward = this.convertValues(source["forward"], ForwardDTO);
	        this.status = this.convertValues(source["status"], StatusInfoDTO);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class SaveAllPayload {
	    domains: DomainDTO[];
	    forwards: ForwardDTO[];
	
	    static createFrom(source: any = {}) {
	        return new SaveAllPayload(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.domains = this.convertValues(source["domains"], DomainDTO);
	        this.forwards = this.convertValues(source["forwards"], ForwardDTO);
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class SaveResultDTO {
	    savedDomains: DomainDTO[];
	    savedForwards: ForwardDTO[];
	    authCancelled: boolean;
	    validationError?: string;
	    otherError?: string;
	
	    static createFrom(source: any = {}) {
	        return new SaveResultDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.savedDomains = this.convertValues(source["savedDomains"], DomainDTO);
	        this.savedForwards = this.convertValues(source["savedForwards"], ForwardDTO);
	        this.authCancelled = source["authCancelled"];
	        this.validationError = source["validationError"];
	        this.otherError = source["otherError"];
	    }
	
		convertValues(a: any, classs: any, asMap: boolean = false): any {
		    if (!a) {
		        return a;
		    }
		    if (a.slice && a.map) {
		        return (a as any[]).map(elem => this.convertValues(elem, classs));
		    } else if ("object" === typeof a) {
		        if (asMap) {
		            for (const key of Object.keys(a)) {
		                a[key] = new classs(a[key]);
		            }
		            return a;
		        }
		        return new classs(a);
		    }
		    return a;
		}
	}
	export class SettingsDTO {
	    paused: boolean;
	    dnsRefreshMinutes: number;
	    autoStartState: string;
	
	    static createFrom(source: any = {}) {
	        return new SettingsDTO(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.paused = source["paused"];
	        this.dnsRefreshMinutes = source["dnsRefreshMinutes"];
	        this.autoStartState = source["autoStartState"];
	    }
	}
	export class SimpleResult {
	    ok: boolean;
	    authCancelled: boolean;
	    error?: string;
	
	    static createFrom(source: any = {}) {
	        return new SimpleResult(source);
	    }
	
	    constructor(source: any = {}) {
	        if ('string' === typeof source) source = JSON.parse(source);
	        this.ok = source["ok"];
	        this.authCancelled = source["authCancelled"];
	        this.error = source["error"];
	    }
	}

}

